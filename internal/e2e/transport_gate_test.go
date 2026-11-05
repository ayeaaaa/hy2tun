package e2e

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const maxGateUDPPeers = 128

// transportGate is a test-only loopback middlebox. It owns one numeric port on
// both TCP and UDP and forwards both to the same real server address. UDP
// forwarding can be disabled without touching the TCP listener, which lets E2E
// tests model a network that blocks QUIC but still permits the TCP fallback.
type transportGate struct {
	backend string

	tcpListener net.Listener
	udpConn     *net.UDPConn
	addr        string

	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error

	wg sync.WaitGroup

	mu       sync.Mutex
	closing  bool
	tcpConns map[net.Conn]struct{}
	udpPeers map[string]*gateUDPPeer

	// udpMu is the disable barrier. Forward/reply writes hold it for read while
	// SetUDPEnabled holds it for write, so a disable that returns can no longer
	// race a datagram or reply onto the wire. Lock order: udpMu -> mu.
	udpMu      sync.RWMutex
	udpEnabled bool

	hookMu          sync.Mutex
	testForwardHook func()
	testReplyHook   func()

	forwarded atomic.Int64
	replied   atomic.Int64
}

type gateUDPPeer struct {
	key      string
	client   *net.UDPAddr
	upstream *net.UDPConn
	last     atomic.Int64
	once     sync.Once
}

func (p *gateUDPPeer) close() {
	p.once.Do(func() { _ = p.upstream.Close() })
}

// newTransportGate binds the same numeric loopback port for TCP and UDP and
// forwards both to backend. The gate starts with UDP enabled; tests that need a
// blocked path call SetUDPEnabled(false) before starting a client.
func newTransportGate(t testing.TB, backend string) *transportGate {
	t.Helper()

	host, _, err := net.SplitHostPort(backend)
	if err != nil {
		t.Fatalf("transport gate backend %q: %v", backend, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("transport gate backend %q is not loopback", backend)
	}

	for attempt := 0; attempt < 20; attempt++ {
		tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("transport gate bind TCP: %v", err)
		}
		_, port, err := net.SplitHostPort(tcpListener.Addr().String())
		if err != nil {
			_ = tcpListener.Close()
			t.Fatalf("transport gate parse TCP address: %v", err)
		}
		udpPort := 0
		if _, err := fmt.Sscanf(port, "%d", &udpPort); err != nil {
			_ = tcpListener.Close()
			t.Fatalf("transport gate parse TCP port %q: %v", port, err)
		}
		udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: udpPort})
		if err != nil {
			_ = tcpListener.Close()
			continue
		}

		g := &transportGate{
			backend:     backend,
			tcpListener: tcpListener,
			udpConn:     udpConn,
			addr:        tcpListener.Addr().String(),
			closed:      make(chan struct{}),
			tcpConns:    make(map[net.Conn]struct{}),
			udpPeers:    make(map[string]*gateUDPPeer),
			udpEnabled:  true,
		}

		g.wg.Add(2)
		go g.acceptTCP()
		go g.forwardUDP()

		t.Cleanup(func() {
			if err := g.Close(); err != nil {
				t.Errorf("close transport gate: %v", err)
			}
		})
		return g
	}
	t.Fatal("transport gate could not bind matching TCP and UDP loopback ports")
	return nil
}

// Addr returns the gate's numeric TCP/UDP loopback address.
func (g *transportGate) Addr() string {
	if g == nil {
		return ""
	}
	return g.addr
}

// SetUDPEnabled toggles UDP forwarding. Disabling drops new UDP datagrams and
// closes existing upstream sockets; TCP connections and the TCP listener stay
// untouched.
func (g *transportGate) SetUDPEnabled(enabled bool) {
	if g == nil {
		return
	}
	g.udpMu.Lock()
	g.udpEnabled = enabled
	if !enabled {
		g.closeUDPPeers()
	}
	g.udpMu.Unlock()
}

// setTestHooks installs optional callbacks that run while the gate holds its
// read barrier, immediately before an upstream forward or client reply. Tests
// use them to hold a datagram in flight and prove disable is ordered after it.
func (g *transportGate) setTestHooks(forward, reply func()) {
	g.hookMu.Lock()
	g.testForwardHook, g.testReplyHook = forward, reply
	g.hookMu.Unlock()
}

func (g *transportGate) hooks() (forward, reply func()) {
	g.hookMu.Lock()
	defer g.hookMu.Unlock()
	return g.testForwardHook, g.testReplyHook
}

// Close stops both forwarders, closes every client/upstream socket and waits
// for all gate goroutines to exit.
func (g *transportGate) Close() error {
	if g == nil {
		return nil
	}
	g.closeOnce.Do(func() {
		close(g.closed)
		var errs []error
		if err := g.tcpListener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
		if err := g.udpConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}

		g.mu.Lock()
		g.closing = true
		conns := make([]net.Conn, 0, len(g.tcpConns))
		for conn := range g.tcpConns {
			conns = append(conns, conn)
		}
		peers := make([]*gateUDPPeer, 0, len(g.udpPeers))
		for _, peer := range g.udpPeers {
			peers = append(peers, peer)
		}
		g.udpPeers = make(map[string]*gateUDPPeer)
		g.mu.Unlock()

		for _, conn := range conns {
			_ = conn.Close()
		}
		for _, peer := range peers {
			peer.close()
		}
		g.wg.Wait()
		g.closeErr = errors.Join(errs...)
	})
	return g.closeErr
}

func (g *transportGate) acceptTCP() {
	defer g.wg.Done()
	for {
		conn, err := g.tcpListener.Accept()
		if err != nil {
			return
		}
		g.mu.Lock()
		if g.closing {
			g.mu.Unlock()
			_ = conn.Close()
			return
		}
		g.tcpConns[conn] = struct{}{}
		g.wg.Add(1)
		g.mu.Unlock()
		go g.forwardTCP(conn)
	}
}

func (g *transportGate) forwardTCP(client net.Conn) {
	defer g.wg.Done()
	defer func() {
		g.mu.Lock()
		delete(g.tcpConns, client)
		g.mu.Unlock()
		_ = client.Close()
	}()

	upstream, err := net.DialTimeout("tcp", g.backend, 5*time.Second)
	if err != nil {
		return
	}
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		_ = upstream.Close()
		return
	}
	g.tcpConns[upstream] = struct{}{}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.tcpConns, upstream)
		g.mu.Unlock()
		_ = upstream.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, client)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		if tcp, ok := client.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	wg.Wait()
}

func (g *transportGate) forwardUDP() {
	defer g.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, client, err := g.udpConn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		g.udpMu.RLock()
		if !g.udpEnabled {
			g.udpMu.RUnlock()
			continue
		}
		forwardHook, _ := g.hooks()
		if forwardHook != nil {
			forwardHook()
		}
		peer := g.peer(client)
		if peer != nil {
			if _, err := peer.upstream.Write(buf[:n]); err != nil {
				g.removePeer(peer)
			} else {
				g.forwarded.Add(1)
			}
		}
		g.udpMu.RUnlock()
	}
}

func (g *transportGate) peer(client *net.UDPAddr) *gateUDPPeer {
	key := client.String()

	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		return nil
	}
	if peer := g.udpPeers[key]; peer != nil {
		peer.last.Store(time.Now().UnixNano())
		g.mu.Unlock()
		return peer
	}

	if len(g.udpPeers) >= maxGateUDPPeers {
		var victim *gateUDPPeer
		for _, candidate := range g.udpPeers {
			if victim == nil || candidate.last.Load() < victim.last.Load() {
				victim = candidate
			}
		}
		if victim != nil {
			delete(g.udpPeers, victim.key)
			victim.close()
		}
	}

	backend, err := net.ResolveUDPAddr("udp", g.backend)
	if err != nil {
		g.mu.Unlock()
		return nil
	}
	upstream, err := net.DialUDP("udp", nil, backend)
	if err != nil {
		g.mu.Unlock()
		return nil
	}
	peer := &gateUDPPeer{key: key, client: client, upstream: upstream}
	peer.last.Store(time.Now().UnixNano())
	g.udpPeers[key] = peer
	g.wg.Add(1)
	go g.replyUDP(peer)
	g.mu.Unlock()
	return peer
}

func (g *transportGate) replyUDP(peer *gateUDPPeer) {
	defer g.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, err := peer.upstream.Read(buf)
		if err != nil {
			g.removePeer(peer)
			return
		}
		g.udpMu.RLock()
		if !g.udpEnabled {
			g.udpMu.RUnlock()
			continue
		}
		_, replyHook := g.hooks()
		if replyHook != nil {
			replyHook()
		}
		_, err = g.udpConn.WriteToUDP(buf[:n], peer.client)
		if err == nil {
			g.replied.Add(1)
		}
		g.udpMu.RUnlock()
		if err != nil {
			g.removePeer(peer)
			return
		}
		peer.last.Store(time.Now().UnixNano())
	}
}

func (g *transportGate) removePeer(peer *gateUDPPeer) {
	g.mu.Lock()
	if g.udpPeers[peer.key] == peer {
		delete(g.udpPeers, peer.key)
	}
	g.mu.Unlock()
	peer.close()
}

func (g *transportGate) closeUDPPeers() {
	g.mu.Lock()
	peers := make([]*gateUDPPeer, 0, len(g.udpPeers))
	for _, peer := range g.udpPeers {
		peers = append(peers, peer)
	}
	g.udpPeers = make(map[string]*gateUDPPeer)
	g.mu.Unlock()
	for _, peer := range peers {
		peer.close()
	}
}

// TestTransportGateForwardsTCPAndGatesUDP verifies the gate contract directly:
// the same numeric port is owned for both protocols, TCP survives UDP blocking,
// UDP resumes after re-enabling, concurrent source peers keep their replies,
// and Close releases both sockets.
func TestTransportGateForwardsTCPAndGatesUDP(t *testing.T) {
	backend := startDualEcho(t)
	gate := newTransportGate(t, backend)

	// The gate itself owns both numeric sides.
	if ln, err := net.Listen("tcp", gate.Addr()); err == nil {
		_ = ln.Close()
		t.Fatal("gate must own its TCP port")
	}
	if pc, err := net.ListenPacket("udp", gate.Addr()); err == nil {
		_ = pc.Close()
		t.Fatal("gate must own its UDP port")
	}

	require.NoError(t, tcpEcho(gate.Addr(), "gate-tcp-enabled"))
	require.NoError(t, udpEcho(gate.Addr(), "gate-udp-enabled"))

	// Multiple source ports must each receive their own reply.
	const peers = 8
	errs := make(chan error, peers)
	var wg sync.WaitGroup
	for i := 0; i < peers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- udpEcho(gate.Addr(), fmt.Sprintf("gate-peer-%d", i))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	gate.SetUDPEnabled(false)
	require.NoError(t, tcpEcho(gate.Addr(), "gate-tcp-while-udp-blocked"))
	require.Error(t, udpEcho(gate.Addr(), "gate-udp-blocked"),
		"blocked UDP must not be forwarded")

	gate.SetUDPEnabled(true)
	require.Eventually(t, func() bool {
		return udpEcho(gate.Addr(), "gate-udp-recovered") == nil
	}, 3*time.Second, 20*time.Millisecond)

	require.NoError(t, gate.Close())
	ln, err := net.Listen("tcp", gate.Addr())
	require.NoError(t, err, "Close must release the TCP port")
	_ = ln.Close()
	pc, err := net.ListenPacket("udp", gate.Addr())
	require.NoError(t, err, "Close must release the UDP port")
	_ = pc.Close()
}

// TestTransportGateDisableIsOrderedWithTraffic proves SetUDPEnabled(false) is a
// strict barrier: an admitted forward/reply completes before disable returns,
// and no UDP packet crosses afterwards. TCP must remain untouched.
func TestTransportGateDisableIsOrderedWithTraffic(t *testing.T) {
	backend := startDualEcho(t)
	gate := newTransportGate(t, backend)

	send := func() {
		t.Helper()
		conn, err := net.Dial("udp", gate.Addr())
		require.NoError(t, err)
		defer conn.Close()
		_, err = conn.Write([]byte("ordered"))
		require.NoError(t, err)
	}

	// Forward direction: hold the gate after it admits a datagram but before it
	// writes upstream, then disable concurrently.
	forwardEntered := make(chan struct{}, 1)
	forwardRelease := make(chan struct{})
	var forwardOnce sync.Once
	releaseForward := func() { forwardOnce.Do(func() { close(forwardRelease) }) }
	t.Cleanup(releaseForward)
	gate.setTestHooks(func() {
		select {
		case forwardEntered <- struct{}{}:
		default:
		}
		<-forwardRelease
	}, nil)

	send()
	requireGateHook(t, forwardEntered, "forward")
	forwardDisable := startGateDisable(gate)
	requireGateDisableBlocked(t, forwardDisable, "forward")
	releaseForward()
	requireGateDisableDone(t, forwardDisable, "forward")

	forwarded := gate.forwarded.Load()
	require.NotZero(t, forwarded, "the admitted forward must complete before disable returns")
	send()
	require.Never(t, func() bool { return gate.forwarded.Load() != forwarded },
		200*time.Millisecond, 10*time.Millisecond, "no UDP forward may cross after disable returns")
	require.NoError(t, tcpEcho(gate.Addr(), "tcp-survives-forward-disable"))

	// Reply direction: enable again, hold an admitted reply, then disable.
	gate.SetUDPEnabled(true)
	gate.setTestHooks(nil, nil)
	replyEntered := make(chan struct{}, 1)
	replyRelease := make(chan struct{})
	var replyOnce sync.Once
	releaseReply := func() { replyOnce.Do(func() { close(replyRelease) }) }
	t.Cleanup(releaseReply)
	gate.setTestHooks(nil, func() {
		select {
		case replyEntered <- struct{}{}:
		default:
		}
		<-replyRelease
	})

	send()
	requireGateHook(t, replyEntered, "reply")
	replyDisable := startGateDisable(gate)
	requireGateDisableBlocked(t, replyDisable, "reply")
	releaseReply()
	requireGateDisableDone(t, replyDisable, "reply")

	replied := gate.replied.Load()
	require.NotZero(t, replied, "the admitted reply must complete before disable returns")
	send()
	require.Never(t, func() bool { return gate.replied.Load() != replied },
		200*time.Millisecond, 10*time.Millisecond, "no UDP reply may cross after disable returns")
	require.NoError(t, tcpEcho(gate.Addr(), "tcp-survives-reply-disable"))
}

func startGateDisable(gate *transportGate) <-chan struct{} {
	done := make(chan struct{})
	started := make(chan struct{})
	go func() {
		close(started)
		gate.SetUDPEnabled(false)
		close(done)
	}()
	<-started
	return done
}

func requireGateHook(t *testing.T, entered <-chan struct{}, direction string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("the gate never admitted the %s datagram", direction)
	}
}

func requireGateDisableBlocked(t *testing.T, done <-chan struct{}, direction string) {
	t.Helper()
	select {
	case <-done:
		t.Fatalf("SetUDPEnabled(false) returned while a %s write was still in flight", direction)
	case <-time.After(100 * time.Millisecond):
	}
}

func requireGateDisableDone(t *testing.T, done <-chan struct{}, direction string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("SetUDPEnabled(false) did not return after the %s write completed", direction)
	}
}

// startDualEcho starts loopback TCP and UDP echo servers on the same numeric
// port. It returns that shared address.
func startDualEcho(t testing.TB) string {
	t.Helper()

	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("start dual echo TCP: %v", err)
	}
	_, port, err := net.SplitHostPort(tcpListener.Addr().String())
	if err != nil {
		_ = tcpListener.Close()
		t.Fatalf("parse dual echo TCP address: %v", err)
	}
	udpPort := 0
	if _, err := fmt.Sscanf(port, "%d", &udpPort); err != nil {
		_ = tcpListener.Close()
		t.Fatalf("parse dual echo TCP port %q: %v", port, err)
	}
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: udpPort})
	if err != nil {
		_ = tcpListener.Close()
		t.Fatalf("start dual echo UDP: %v", err)
	}

	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		conns      = make(map[net.Conn]struct{})
		acceptDone = make(chan struct{})
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer close(acceptDone)
		for {
			conn, err := tcpListener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns[conn] = struct{}{}
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					mu.Lock()
					delete(conns, conn)
					mu.Unlock()
					_ = conn.Close()
				}()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 65535)
		for {
			n, from, err := udpConn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if _, err := udpConn.WriteToUDP(buf[:n], from); err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		_ = tcpListener.Close()
		_ = udpConn.Close()
		// Join the accept loop before closing accepted connections: no handler
		// can be added to wg after acceptDone closes, so wg.Wait cannot race Add.
		<-acceptDone
		mu.Lock()
		for conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return tcpListener.Addr().String()
}
