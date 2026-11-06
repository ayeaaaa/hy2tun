package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/transport"
)

// Pause a real control child after initial validation, before admission.
type admissionHelloGate struct {
	net.Conn
	reached, resume chan struct{}
	once            sync.Once
}

func (c *admissionHelloGate) SetReadDeadline(at time.Time) error {
	if at.IsZero() {
		c.once.Do(func() { close(c.reached); <-c.resume })
	}
	return c.Conn.SetReadDeadline(at)
}

func TestTCPPeerInvalidatedHelloPreservesHealthyOwner(t *testing.T) {
	for _, invalidate := range []string{"closed", "revoked", "queued_revoke", "deadline", "mode"} {
		t.Run(invalidate, func(t *testing.T) {
			s, tlsConfig := tcpTestServer(t)
			oldToken := s.currentCredentials().ControlToken
			if invalidate == "revoked" || invalidate == "queued_revoke" {
				_, _, err := s.RotateCredentials("new-password", "new-control-token-12345678", "manual")
				require.NoError(t, err)
			}
			set := s.currentCredentials()
			healthy := tcpTestDial(t, tcpTestEndpoint(t, s, tlsConfig), "owner", set.Auth, set.ControlToken)
			healthyControl, _ := tcpTestRegister(t, healthy, "owner", set.ControlToken,
				config.Mapping{ID: "tcp", Type: "tcp", LocalAddr: "127.0.0.1:1", Enabled: true})
			owner := liveTCPTestSession(t, s, "owner")
			s.mappings.mu.Lock()
			rt := owner.mappings["tcp"]
			s.mappings.mu.Unlock()

			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			reached, resume := make(chan struct{}), make(chan struct{})
			var resumeOnce sync.Once
			release := func() { resumeOnce.Do(func() { close(resume) }) }
			defer release()
			peers := make(chan *transport.TCPPeer, 1)
			finished := make(chan error, 1)
			deadline := time.Now().Add(2 * time.Second)
			go func() {
				raw, err := listener.Accept()
				if err != nil {
					finished <- err
					return
				}
				peer, err := transport.AcceptTCP(ctx, raw, s.tcpServerOptions(tlsConfig))
				if err != nil {
					finished <- err
					return
				}
				defer peer.Close()
				peers <- peer
				header, conn, err := peer.Accept(ctx)
				if err != nil {
					finished <- err
					return
				}
				if header.Kind != transport.StreamControl {
					finished <- errors.New("not control")
					return
				}
				if err := transport.WriteStreamAck(conn, nil); err != nil {
					finished <- err
					return
				}
				principal := peer.Principal()
				s.control.serveWithMeta(&admissionHelloGate{Conn: conn, reached: reached, resume: resume}, sessionMeta{
					Kind: transport.TCP, Principal: &principal, Peer: peer, CloseTransport: peer.Close, HandshakeDeadline: deadline,
				})
				finished <- nil
			}()
			queued := tcpTestDial(t, listener.Addr().String(), "owner", "pw", oldToken)
			peer := <-peers
			conn, err := queued.OpenControl(ctx)
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, control.WriteMessage(conn, &control.Hello{Type: control.TypeHello, Protocol: control.ProtocolVersion, ClientID: "owner", Token: oldToken}))
			select {
			case <-reached:
			case <-ctx.Done():
				t.Fatal("Hello did not reach admission boundary")
			}
			switch invalidate {
			case "closed":
				require.NoError(t, peer.Close())
			case "revoked":
				require.NoError(t, s.RevokePreviousCredentials())
			case "queued_revoke":
				s.mappings.mu.Lock()
				locked := true
				defer func() {
					if locked {
						s.mappings.mu.Unlock()
					}
				}()
				release()
				time.Sleep(20 * time.Millisecond)
				revoked := make(chan error, 1)
				go func() { revoked <- s.RevokePreviousCredentials() }()
				// A Hello queued for the manager must not retain a settings read
				// lock that prevents the credential invalidation from committing.
				require.Eventually(t, func() bool { return s.currentCredentials().PreviousGeneration == 0 }, time.Second, time.Millisecond)
				s.mappings.mu.Unlock()
				locked = false
				require.NoError(t, <-revoked)
			case "deadline":
				time.Sleep(time.Until(deadline) + time.Millisecond)
			case "mode":
				s.settingsMu.Lock()
				next := cloneConfig(s.cfg)
				next.Mode = config.ModeHY2
				s.cfg = next
				s.settingsMu.Unlock()
			}
			release()
			select {
			case err := <-finished:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("rejected Hello did not finish")
			}
			s.mappings.mu.Lock()
			preserved := s.mappings.sessions["owner"] == owner
			s.mappings.mu.Unlock()
			require.True(t, preserved, "invalidated Hello evicted independently healthy same-ID owner")
			require.False(t, rt.closed.Load(), "healthy owner's public listener was closed")
			select {
			case <-owner.done:
				t.Fatal("healthy owner was detached")
			default:
			}
			require.NoError(t, healthyControl.SetDeadline(time.Now().Add(time.Second)))
			require.NoError(t, controlPingForTest(healthyControl), "healthy control must remain usable")
			require.Same(t, rt.listeners.TCP[0], owner.mappings["tcp"].listeners.TCP[0])
		})
	}
}

func attachedFixUDP(t *testing.T) (*Server, transport.Session, transport.DatagramConn, *mappingRuntime, *publicUDPConn) {
	t.Helper()
	s, tlsConfig := tcpTestServer(t)
	root := tcpTestDial(t, tcpTestEndpoint(t, s, tlsConfig), "owner", "pw", s.currentCredentials().ControlToken)
	_, ack := tcpTestRegister(t, root, "owner", s.currentCredentials().ControlToken,
		config.Mapping{ID: "udp", Type: "udp", LocalAddr: "127.0.0.1:1", Enabled: true})
	owner := liveTCPTestSession(t, s, "owner")
	s.mappings.mu.Lock()
	rt := owner.mappings["udp"]
	s.mappings.mu.Unlock()
	d, err := root.OpenUDP(context.Background(), ack.Results[0].UDPToken)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	var public *publicUDPConn
	require.Eventually(t, func() bool { rt.udpMu.Lock(); defer rt.udpMu.Unlock(); public = rt.udpSession; return public != nil }, time.Second, time.Millisecond)
	return s, root, d, rt, public
}

func TestTCPUDPRootCleanupDoesNotHoldManagerDuringDNS(t *testing.T) {
	s, root, d, rt, _ := attachedFixUDP(t)
	started, release := make(chan struct{}), make(chan struct{})
	var startOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	original := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		startOnce.Do(func() { close(started) })
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, fmt.Errorf("test DNS released")
		}
	}}
	defer func() { unblock(); net.DefaultResolver = original }()
	require.NoError(t, d.Send([]byte("packet"), "task4-fix.invalid:9999"))
	childDone := make(chan error, 1)
	go func() { _, _, err := d.Receive(); childDone <- err }()
	// Old implementation starts a delayed lookup; a numeric-only implementation
	// rejects the address and closes its child without starting DNS at all.
	lookup := false
	select {
	case <-started:
		lookup = true
	case err := <-childDone:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("peer address neither rejected nor processed")
	}
	require.NoError(t, root.Close())
	require.Eventually(t, func() bool { return rt.closed.Load() }, time.Second, time.Millisecond)
	managerDone := make(chan struct{})
	go func() { s.mappings.clientCount(); close(managerDone) }()
	select {
	case <-managerDone:
	case <-time.After(150 * time.Millisecond):
		t.Error("unrelated manager access blocked by closed root's delayed DNS")
	}
	if lookup {
		t.Error("reverse UDP peer metadata reached DNS instead of numeric parsing")
	}
	unblock()
	select {
	case <-managerDone:
	case <-time.After(time.Second):
		t.Fatal("manager did not recover")
	}
}

func TestTCPUDPRootCleanupDoesNotDrainUnderManager(t *testing.T) {
	s, root, _, rt, public := attachedFixUDP(t)
	// Delay the real wrapper's close, independently of the address policy. The
	// manager must remain available even if a child is slow to drain.
	public.mu.Lock()
	locked := true
	defer func() {
		if locked {
			public.mu.Unlock()
		}
	}()
	require.NoError(t, root.Close())
	require.Eventually(t, func() bool { return rt.closed.Load() }, time.Second, time.Millisecond)
	managerDone := make(chan struct{})
	go func() { s.mappings.clientCount(); close(managerDone) }()
	select {
	case <-managerDone:
	case <-time.After(150 * time.Millisecond):
		t.Error("shared manager held while UDP child close waits")
	}
	public.mu.Unlock()
	locked = false
	select {
	case <-managerDone:
	case <-time.After(time.Second):
		t.Fatal("manager did not recover after drain released")
	}
}

func TestPublicUDPConnNumericIPv6AndZone(t *testing.T) {
	public, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1")})
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	defer public.Close()
	peer, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.ParseIP("::1")})
	require.NoError(t, err)
	defer peer.Close()
	interfaces, err := net.Interfaces()
	require.NoError(t, err)
	addresses := []string{peer.LocalAddr().String()}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback != 0 {
			addresses = append(addresses, net.JoinHostPort("::1%"+iface.Name, strconv.Itoa(peer.LocalAddr().(*net.UDPAddr).Port)))
			break
		}
	}
	wrapper := &publicUDPConn{UDPConn: public}
	for _, addr := range addresses {
		_, err := wrapper.WriteTo([]byte("numeric peer"), addr)
		require.NoError(t, err, "numeric IPv6 peer %q", addr)
		require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
		buf := make([]byte, 30)
		n, _, err := peer.ReadFromUDP(buf)
		require.NoError(t, err)
		require.Equal(t, "numeric peer", string(buf[:n]))
	}
}

func TestTCPPeerTokenOnlyRotationStillAdmitsBothPairs(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	addr := tcpTestEndpoint(t, s, tlsConfig)
	oldToken := s.currentCredentials().ControlToken
	_, _, err := s.RotateCredentials("", "replacement-token-1234567890", "manual")
	require.NoError(t, err)
	previous := tcpTestDial(t, addr, "previous", "pw", oldToken)
	tcpTestRegister(t, previous, "previous", oldToken)
	old := liveTCPTestSession(t, s, "previous")
	current := tcpTestDial(t, addr, "current", "pw", s.currentCredentials().ControlToken)
	controlConn, _ := tcpTestRegister(t, current, "current", s.currentCredentials().ControlToken)
	live := liveTCPTestSession(t, s, "current")
	oldGen, _, _ := old.authenticatedCredentialState()
	currentGen, _, _ := live.authenticatedCredentialState()
	require.Equal(t, int64(1), oldGen)
	require.Equal(t, int64(2), currentGen)
	require.NoError(t, s.RevokePreviousCredentials())
	require.Eventually(t, func() bool { _, err := previous.Probe(context.Background()); return err != nil }, time.Second, time.Millisecond)
	require.NoError(t, controlConn.SetDeadline(time.Now().Add(time.Second)))
	require.NoError(t, controlPingForTest(controlConn))
}
