package client

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/testutil"
	"hy2tun/internal/transport"
)

// sessionStub replaces only the network boundary; the client handshake,
// ownership, cancellation and mapping/proxy routing remain real.
type sessionStub struct {
	kind    transport.Kind
	control net.Conn
	tcp     func(context.Context, transport.TCPRequest) (net.Conn, error)
	udp     transport.DatagramConn
	closed  chan struct{}
	once    sync.Once
}

func newSessionStub(kind transport.Kind) *sessionStub {
	return &sessionStub{kind: kind, closed: make(chan struct{})}
}
func (s *sessionStub) Kind() transport.Kind                          { return s.kind }
func (s *sessionStub) OpenControl(context.Context) (net.Conn, error) { return s.control, nil }
func (s *sessionStub) OpenTCP(ctx context.Context, req transport.TCPRequest) (net.Conn, error) {
	if s.tcp == nil {
		return nil, errors.New("unexpected TCP open")
	}
	return s.tcp(ctx, req)
}
func (s *sessionStub) OpenUDP(context.Context, string) (transport.DatagramConn, error) {
	return s.udp, nil
}
func (s *sessionStub) Probe(context.Context) (transport.ProbeInfo, error) {
	return transport.ProbeInfo{}, nil
}
func (s *sessionStub) Close() error {
	s.once.Do(func() {
		close(s.closed)
		if s.control != nil {
			_ = s.control.Close()
		}
		if s.udp != nil {
			_ = s.udp.Close()
		}
	})
	return nil
}

func TestForcedTCPNoUDPDial(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Mode = config.ModeTCP
	// This host is deliberately unresolvable. Preparing TCP must not resolve UDP.
	cfg.Server = "unresolvable.invalid:443"
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer cl.Close()
	cl.newHY2 = func(*hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error) {
		t.Error("forced TCP attempted HY2")
		return nil, nil, errors.New("wrong transport")
	}
	var kinds []transport.Kind
	cl.newSession = func(ctx context.Context, kind transport.Kind, opts transport.DialOptions) (transport.Session, error) {
		kinds = append(kinds, kind)
		require.Nil(t, opts.HY2)
		require.Equal(t, "unresolvable.invalid:443", opts.TCPAddress)
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), 10*time.Second)
		return nil, io.EOF
	}
	connected, err := cl.runOnce(context.Background())
	require.False(t, connected)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, []transport.Kind{transport.TCP}, kinds)
}

func TestForcedHY2NeverTCP(t *testing.T) {
	for _, mode := range []config.TransportMode{"", config.ModeHY2} {
		name := string(mode)
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			cfg := validTestClientConfig()
			cfg.Mode = mode
			cl, err := New(cfg, nil)
			require.NoError(t, err)
			defer cl.Close()
			var kinds []transport.Kind
			cl.newSession = func(_ context.Context, kind transport.Kind, opts transport.DialOptions) (transport.Session, error) {
				kinds = append(kinds, kind)
				require.NotNil(t, opts.HY2)
				return nil, io.EOF
			}
			_, err = cl.runOnce(context.Background())
			require.ErrorIs(t, err, io.EOF)
			require.Equal(t, []transport.Kind{transport.HY2}, kinds)
		})
	}
}

func TestTransportCloseUnblocksUDPAndDials(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	s := newSessionStub(transport.TCP)
	s.udp = newFakeUDPConn()
	owner, peer := net.Pipe()
	defer peer.Close()
	require.True(t, cl.setCurrent(s, owner))
	cl.mappingConfigs["tcp"] = config.Mapping{ID: "tcp", Type: "tcp", LocalAddr: "127.0.0.1:9", Enabled: true}
	started := make(chan struct{})
	cl.dialLocal = func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		cl.handleOpen(owner, &control.Open{MappingID: "tcp", Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
		close(done)
	}()
	<-started
	u, err := cl.startUDP(config.Mapping{ID: "udp", Type: "udp", LocalAddr: "127.0.0.1:9", Enabled: true}, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	cl.udpMappings["udp"] = u
	// Block a UDP admission, then close. Session must close before mapping waits.
	f := s.udp.(*fakeUDPConn)
	for i := 0; i < cap(f.sent); i++ {
		f.sent <- testUDPMessage{}
	}
	sendStarted := make(chan struct{})
	f.sendFn = func() { close(sendStarted) }
	sendDone := make(chan error, 1)
	go func() { sendDone <- u.send([]byte("x"), "127.0.0.1:8") }()
	<-sendStarted
	require.NoError(t, cl.Close())
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("local dial survived session shutdown")
	}
	select {
	case <-sendDone:
	case <-time.After(time.Second):
		t.Fatal("UDP send survived session shutdown")
	}
}

func TestOldOwnerCannotTrackDataAfterSwitch(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	old, oldPeer := net.Pipe()
	defer oldPeer.Close()
	defer old.Close()
	oldSession := newSessionStub(transport.HY2)
	require.True(t, cl.setCurrent(oldSession, old))
	newOwner, newPeer := net.Pipe()
	defer newPeer.Close()
	newSession := newSessionStub(transport.TCP)
	require.True(t, cl.setCurrent(newSession, newOwner))
	cl.clearCurrent(oldSession, old)
	cl.markConnected()
	cl.mappingConfigs["tcp"] = config.Mapping{ID: "tcp", Type: "tcp", LocalAddr: "127.0.0.1:9", Enabled: true}
	cl.dialLocal = func(context.Context, string, string) (net.Conn, error) {
		t.Error("stale open dialed local target")
		return nil, io.EOF
	}
	cl.handleOpen(old, &control.Open{MappingID: "tcp", Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	require.Same(t, newSession, cl.currentSession())
	require.True(t, cl.Status().Connected)
	local, peer := net.Pipe()
	defer local.Close()
	defer peer.Close()
	require.False(t, cl.trackData(old, local))
	require.Empty(t, cl.dataConns)
}

func TestTCPProxyUsesCurrentTransport(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	s := newSessionStub(transport.TCP)
	owner, ownerPeer := net.Pipe()
	defer ownerPeer.Close()
	require.True(t, cl.setCurrent(s, owner))
	remote, peer := net.Pipe()
	defer peer.Close()
	s.tcp = func(_ context.Context, req transport.TCPRequest) (net.Conn, error) {
		require.Equal(t, transport.TCPRequest{Target: "example.com:443"}, req)
		return remote, nil
	}
	conn, err := cl.TCPContext(context.Background(), "example.com:443")
	require.NoError(t, err)
	go func() { _, _ = peer.Write([]byte("tcp")) }()
	buf := make([]byte, 3)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "tcp", string(buf))
	cl.closeDataFor(owner)
	_, err = conn.Read(buf)
	require.Error(t, err, "proxy stream must not survive its owner")
}

func TestHelloAckSecurityErrorRetainsClass(t *testing.T) {
	for _, code := range []string{"unauthorized", "unsupported_protocol", "bad_message", "unknown_rejection"} {
		t.Run(code, func(t *testing.T) {
			cl, err := New(validTestClientConfig(), nil)
			require.NoError(t, err)
			defer cl.Close()
			conn, peer := net.Pipe()
			defer conn.Close()
			defer peer.Close()
			go func() {
				_, _ = control.ReadMessage(peer)
				_ = control.WriteMessage(peer, &control.HelloAck{Type: control.TypeHelloAck, Error: code})
			}()
			err = cl.handshake(conn)
			want := transport.ErrProtocol
			if code == "unauthorized" {
				want = transport.ErrAuthentication
			}
			require.ErrorIs(t, err, want)
			require.False(t, transport.FallbackAllowed(errors.Join(err, io.EOF)))
		})
	}
}

func TestMalformedControlRetainsProtocolClass(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	go func() {
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], control.MaxFrameSize+1)
		_, _ = peer.Write(header[:])
	}()
	err = cl.serveControl(conn)
	require.ErrorIs(t, err, transport.ErrProtocol)
	require.False(t, transport.FallbackAllowed(errors.Join(err, io.EOF)))
}

func TestSessionHY2PinRetainsClass(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.TLS.PinSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tls, err := buildTLSConfig(cfg)
	require.NoError(t, err)
	err = tls.VerifyPeerCertificate([][]byte{[]byte("wrong certificate")}, nil)
	require.ErrorIs(t, err, transport.ErrCertificatePin)
	require.False(t, transport.FallbackAllowed(errors.Join(err, io.EOF)))
}

func TestReverseLocalConnectionRemainsOwnedWhilePiping(t *testing.T) {
	echo, _ := testutil.StartTCPEcho(t)
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	s := newSessionStub(transport.TCP)
	owner, ownerPeer := net.Pipe()
	defer ownerPeer.Close()
	require.True(t, cl.setCurrent(s, owner))
	cl.mappingConfigs["tcp"] = config.Mapping{ID: "tcp", Type: "tcp", LocalAddr: echo, Enabled: true}
	remote, peer := net.Pipe()
	defer peer.Close()
	s.tcp = func(context.Context, transport.TCPRequest) (net.Conn, error) { return remote, nil }
	ack := make(chan any, 1)
	go func() { msg, _ := control.ReadMessage(ownerPeer); ack <- msg }()
	cl.handleOpen(owner, &control.Open{MappingID: "tcp", Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	require.True(t, (<-ack).(*control.OpenResult).OK)
	cl.mu.Lock()
	owned := len(cl.dataConns)
	cl.mu.Unlock()
	require.Equal(t, 2, owned, "both local and remote connection must remain owned for the whole pipe")
	cl.closeDataFor(owner)
	_, err = peer.Read(make([]byte, 1))
	require.Error(t, err)
}

func TestSessionSetupDeadlineDoesNotCloseEstablishedSession(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	s := newSessionStub(transport.TCP)
	// Keep the remote endpoint and perform an ordinary HelloAck.
	conn, peer := net.Pipe()
	s.control = conn
	defer peer.Close()
	setup, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := cl.runSessionWithSetup(context.Background(), setup, s, 1); done <- err }()
	_, err = control.ReadMessage(peer)
	require.NoError(t, err)
	require.NoError(t, control.WriteMessage(peer, &control.HelloAck{Type: control.TypeHelloAck, OK: true}))
	ready, cancelReady := context.WithTimeout(context.Background(), time.Second)
	defer cancelReady()
	require.NoError(t, cl.WaitReady(ready))
	<-setup.Done()
	require.NoError(t, control.WriteMessage(peer, &control.Ping{Type: control.TypePing, TS: 123}))
	msg, err := control.ReadMessage(peer)
	require.NoError(t, err)
	require.Equal(t, int64(123), msg.(*control.Pong).TS)
	require.NoError(t, cl.Close())
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session did not close")
	}
}

func TestSessionSetupBudgetIncludesHelloAck(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	s := newSessionStub(transport.TCP)
	conn, peer := net.Pipe()
	s.control = conn
	defer peer.Close()
	setup, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// Spend most of the setup budget before control starts.
	time.Sleep(60 * time.Millisecond)
	go func() { _, _ = control.ReadMessage(peer) }()
	start := time.Now()
	connected, err := cl.runSessionWithSetup(context.Background(), setup, s, 1)
	require.False(t, connected)
	require.Error(t, err)
	require.Less(t, time.Since(start), 250*time.Millisecond, "HelloAck must not get a new ten-second budget")
	select {
	case <-s.closed:
	default:
		t.Fatal("failed temporary session leaked")
	}
}

func TestSessionCanceledDialClosesLateResource(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	s := newSessionStub(transport.TCP)
	started, release := make(chan struct{}), make(chan struct{})
	cl.newSession = func(context.Context, transport.Kind, transport.DialOptions) (transport.Session, error) {
		close(started)
		<-release
		return s, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := cl.dialSession(ctx, transport.TCP, cl.snapshotConfig()); done <- err }()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	close(release)
	select {
	case <-s.closed:
	case <-time.After(time.Second):
		t.Fatal("late dial resource was not closed")
	}
}

func TestSessionReplacementClosesOnlyOldData(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	old := newSessionStub(transport.TCP)
	owner, ownerPeer := net.Pipe()
	defer ownerPeer.Close()
	require.True(t, cl.setCurrent(old, owner))
	remote, peer := net.Pipe()
	defer peer.Close()
	old.tcp = func(context.Context, transport.TCPRequest) (net.Conn, error) { return remote, nil }
	conn, err := cl.TCP("example.com:443")
	require.NoError(t, err)
	newOwner, newPeer := net.Pipe()
	defer newPeer.Close()
	next := newSessionStub(transport.HY2)
	require.True(t, cl.setCurrent(next, newOwner))
	select {
	case <-old.closed:
	default:
		t.Fatal("replacement did not close old root")
	}
	_, err = conn.Read(make([]byte, 1))
	require.Error(t, err)
	cl.clearCurrent(old, owner)
	require.Same(t, next, cl.currentSession())
	select {
	case <-next.closed:
		t.Fatal("old teardown closed new root")
	default:
	}
}

func TestOldOwnerCannotOpenNewSessionAfterLocalDial(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	old := newSessionStub(transport.TCP)
	owner, ownerPeer := net.Pipe()
	defer ownerPeer.Close()
	require.True(t, cl.setCurrent(old, owner))
	cl.mappingConfigs["tcp"] = config.Mapping{ID: "tcp", Type: "tcp", LocalAddr: "127.0.0.1:9", Enabled: true}
	started, release := make(chan struct{}), make(chan struct{})
	local, peer := net.Pipe()
	defer peer.Close()
	cl.dialLocal = func(context.Context, string, string) (net.Conn, error) { close(started); <-release; return local, nil }
	done := make(chan struct{})
	go func() {
		cl.handleOpen(owner, &control.Open{MappingID: "tcp", Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
		close(done)
	}()
	<-started
	next := newSessionStub(transport.HY2)
	next.tcp = func(context.Context, transport.TCPRequest) (net.Conn, error) {
		t.Error("old open used new session")
		return nil, io.EOF
	}
	newOwner, newPeer := net.Pipe()
	defer newPeer.Close()
	require.True(t, cl.setCurrent(next, newOwner))
	close(release)
	<-done
	_, err = peer.Read(make([]byte, 1))
	require.Error(t, err, "late local dial was not closed")
	require.Empty(t, cl.dataConns)
}

type overflowDatagram struct{ *fakeUDPConn }

func (d overflowDatagram) Send(data []byte, addr string) error {
	if string(data) == "drop" {
		return transport.ErrQueueFull
	}
	return d.fakeUDPConn.Send(data, addr)
}

func TestUDPQueueFullDropsOnlyPacket(t *testing.T) {
	echo, _ := testutil.StartUDPEcho(t)
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	s := newSessionStub(transport.TCP)
	f := newFakeUDPConn()
	s.udp = overflowDatagram{f}
	owner, peer := net.Pipe()
	defer peer.Close()
	require.True(t, cl.setCurrent(s, owner))
	u, err := cl.startUDP(config.Mapping{ID: "udp", Type: "udp", LocalAddr: echo, Enabled: true}, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	require.NoError(t, err)
	defer u.close()
	f.incoming <- testUDPMessage{data: []byte("drop"), addr: "127.0.0.1:1234"}
	require.Eventually(t, func() bool { return u.drops.Load() == 1 }, time.Second, time.Millisecond)
	require.Equal(t, 1, u.peerCount(), "overflow must not remove the public peer")
	f.incoming <- testUDPMessage{data: []byte("keep"), addr: "127.0.0.1:1234"}
	select {
	case msg := <-f.sent:
		require.Equal(t, "keep", string(msg.data))
	case <-time.After(time.Second):
		t.Fatal("later packet was not sent")
	}
	require.Eventually(t, func() bool { return u.traffic.tx.Load() == 4 }, time.Second, time.Millisecond)
	select {
	case <-s.closed:
		t.Fatal("overflow closed the root")
	default:
	}
}

func TestSessionHY2ResolutionHonorsCancellation(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	cfg := cl.snapshotConfig()
	cfg.Server = "task6-dns.invalid:443"
	oldResolver := net.DefaultResolver
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		once.Do(func() { close(started) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, io.EOF
		}
	}}
	defer func() { net.DefaultResolver = oldResolver }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := cl.dialSession(ctx, transport.HY2, cfg); done <- err }()
	<-started
	cancel()
	var returned bool
	select {
	case err = <-done:
		returned = true
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if !returned {
		<-done
	}
	require.True(t, returned, "DNS resolution outlived the establishment context")
	require.ErrorIs(t, err, context.Canceled)
}

type cancelAfterAckConn struct {
	net.Conn
	reads  int
	cancel context.CancelFunc
}

func (c *cancelAfterAckConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.reads++
		if c.reads == 2 {
			c.cancel()
		}
	}
	return n, err
}

func TestSessionSecurityErrorWinsSetupCancellation(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	setup, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, peer := net.Pipe()
	defer peer.Close()
	s := newSessionStub(transport.TCP)
	s.control = &cancelAfterAckConn{Conn: conn, cancel: cancel}
	go func() {
		_, _ = control.ReadMessage(peer)
		_ = control.WriteMessage(peer, &control.HelloAck{Type: control.TypeHelloAck, Error: "unauthorized"})
	}()
	_, err = cl.runSessionWithSetup(context.Background(), setup, s, 1)
	require.ErrorIs(t, err, transport.ErrAuthentication)
	require.False(t, transport.FallbackAllowed(errors.Join(err, context.DeadlineExceeded)))
}

type switchAfterAckConn struct {
	net.Conn
	reads       int
	switchOwner func()
}

func (c *switchAfterAckConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.reads++
		if c.reads == 2 {
			c.switchOwner()
		}
	}
	return n, err
}

func TestOldOwnerHelloAckCannotMarkReplacementConnected(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	old, peer := net.Pipe()
	defer peer.Close()
	newOwner, newPeer := net.Pipe()
	defer newPeer.Close()
	next := newSessionStub(transport.TCP)
	wrapped := &switchAfterAckConn{Conn: old, switchOwner: func() { cl.setCurrent(next, newOwner) }}
	require.True(t, cl.setCurrent(newSessionStub(transport.HY2), wrapped))
	go func() {
		_, _ = control.ReadMessage(peer)
		_ = control.WriteMessage(peer, &control.HelloAck{Type: control.TypeHelloAck, OK: true})
	}()
	err = cl.handshake(wrapped)
	require.ErrorIs(t, err, net.ErrClosed)
	require.Same(t, next, cl.currentSession())
	require.False(t, cl.Status().Connected, "old HelloAck must not mark a new, unhandshaken root connected")
}
