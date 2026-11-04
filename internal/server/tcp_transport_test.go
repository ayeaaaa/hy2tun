package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/testutil"
	"hy2tun/internal/transport"
)

// These tests use the real TLS/mux adapter, without production TCP startup.
func tcpTestServer(t *testing.T) (*Server, *tls.Config) {
	t.Helper()
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	cfg.Mode = config.ModeAuto
	cfg.ListenHost = "127.0.0.1"
	s, err := New(cfg, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s, &tls.Config{Certificates: []tls.Certificate{cert}}
}

func tcpTestEndpoint(t *testing.T, s *Server, tlsConfig *tls.Config) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { _ = s.serveTCPConn(ctx, c, tlsConfig) }()
		}
	}()
	return l.Addr().String()
}

func tcpTestDial(t *testing.T, addr, id, auth, token string) transport.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sess, err := transport.DialTCP(ctx, transport.DialOptions{
		TCPAddress: addr, TLS: &tls.Config{InsecureSkipVerify: true},
		Auth: auth, ControlToken: token, ClientID: id,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func tcpTestRegister(t *testing.T, root transport.Session, id, token string, mappings ...config.Mapping) (net.Conn, *control.HelloAck) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := root.OpenControl(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.SetDeadline(time.Now().Add(3*time.Second)))
	require.NoError(t, control.WriteMessage(c, &control.Hello{
		Type: control.TypeHello, Protocol: control.ProtocolVersion, ClientID: id,
		Token: token, Mappings: mappings,
	}))
	msg, err := control.ReadMessage(c)
	require.NoError(t, err)
	ack, ok := msg.(*control.HelloAck)
	require.True(t, ok)
	require.True(t, ack.OK, "%+v", ack)
	require.NoError(t, c.SetDeadline(time.Time{}))
	return c, ack
}

func liveTCPTestSession(t *testing.T, s *Server, id string) *clientSession {
	t.Helper()
	var owner *clientSession
	require.Eventually(t, func() bool {
		s.mappings.mu.Lock()
		defer s.mappings.mu.Unlock()
		owner = s.mappings.sessions[id]
		return owner != nil && owner.isReady()
	}, time.Second, time.Millisecond)
	return owner
}

func TestTCPPeerRequiresControlFirst(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	root := tcpTestDial(t, tcpTestEndpoint(t, s, tlsConfig), "owner", "pw", s.currentConfig().Control.Token)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	token := "0123456789abcdef0123456789abcdef"
	_, err := root.OpenTCP(ctx, transport.TCPRequest{ReverseToken: token})
	require.ErrorIs(t, err, transport.ErrAuthentication)
	_, err = root.OpenUDP(ctx, token)
	require.ErrorIs(t, err, transport.ErrAuthentication)
	_, err = root.OpenTCP(ctx, transport.TCPRequest{Target: "127.0.0.1:1"})
	require.ErrorIs(t, err, transport.ErrAuthentication)

	c, err := root.OpenControl(ctx)
	require.NoError(t, err)
	defer c.Close()
	_, err = root.OpenTCP(ctx, transport.TCPRequest{Target: "127.0.0.1:1"})
	require.ErrorIs(t, err, transport.ErrAuthentication, "control stream alone is not registration")
	_, err = root.OpenControl(ctx)
	require.ErrorIs(t, err, transport.ErrAuthentication, "one control per root")
	require.NoError(t, control.WriteMessage(c, &control.Hello{Type: control.TypeHello, Protocol: control.ProtocolVersion, ClientID: "owner", Token: token}))
	msg, err := control.ReadMessage(c)
	require.NoError(t, err)
	require.True(t, msg.(*control.HelloAck).OK)
	owner := liveTCPTestSession(t, s, "owner")
	require.Equal(t, transport.TCP, owner.transport)
	require.NotNil(t, owner.tcpPeer)

	_, err = root.OpenTCP(ctx, transport.TCPRequest{Target: "_hy2tun_unknown_:0"})
	require.ErrorIs(t, err, transport.ErrProtocol)
	_, err = root.OpenTCP(ctx, transport.TCPRequest{Target: "_hy2tun_probe_:0"})
	require.ErrorIs(t, err, transport.ErrProtocol)
}

func TestTCPPeerHelloBindsIdentityAndToken(t *testing.T) {
	for _, tc := range []struct{ name, id, token string }{
		{"identity", "other", "0123456789abcdef0123456789abcdef"},
		{"token", "owner", "previous-control-token-12345678"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, tlsConfig := tcpTestServer(t)
			set := s.currentCredentials()
			set.PreviousGeneration = 1
			set.Generation = 2
			set.PreviousControlToken = "previous-control-token-12345678"
			s.credentials.Store(set)
			root := tcpTestDial(t, tcpTestEndpoint(t, s, tlsConfig), "owner", "pw", set.ControlToken)
			c, err := root.OpenControl(context.Background())
			require.NoError(t, err)
			defer c.Close()
			require.NoError(t, c.SetDeadline(time.Now().Add(time.Second)))
			require.NoError(t, control.WriteMessage(c, &control.Hello{Type: control.TypeHello, Protocol: control.ProtocolVersion, ClientID: tc.id, Token: tc.token}))
			msg, err := control.ReadMessage(c)
			if err == nil {
				require.False(t, msg.(*control.HelloAck).OK)
			}
			require.Empty(t, s.Clients())
			require.Eventually(t, func() bool { _, err := root.Probe(context.Background()); return err != nil }, time.Second, time.Millisecond)
		})
	}
}

func TestTCPDataTokenWrongOwnerDoesNotConsume(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	addr := tcpTestEndpoint(t, s, tlsConfig)
	token := s.currentConfig().Control.Token
	root := tcpTestDial(t, addr, "owner", "pw", token)
	tcpTestRegister(t, root, "owner", token, config.Mapping{ID: "tcp", Type: "tcp", LocalAddr: "127.0.0.1:1", Enabled: true})
	owner := liveTCPTestSession(t, s, "owner")
	otherRoot := tcpTestDial(t, addr, "other", "pw", token)
	tcpTestRegister(t, otherRoot, "other", token)
	s.mappings.mu.Lock()
	rt := owner.mappings["tcp"]
	s.mappings.mu.Unlock()
	a, b := net.Pipe()
	defer b.Close()
	dataToken, err := s.data.add(&pendingConn{conn: a, mapping: rt, owner: owner, expires: time.Now().Add(time.Second)})
	require.NoError(t, err)
	_, err = otherRoot.OpenTCP(context.Background(), transport.TCPRequest{ReverseToken: dataToken})
	require.ErrorIs(t, err, transport.ErrAuthentication)
	old := &clientSession{id: "owner", tcpPeer: owner.tcpPeer, transport: transport.TCP}
	_, err = s.data.takeFor(dataToken, old)
	require.Error(t, err)
	c, err := root.OpenTCP(context.Background(), transport.TCPRequest{ReverseToken: dataToken})
	require.NoError(t, err)
	defer c.Close()
	go func() { _, _ = b.Write([]byte("hello")) }()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(time.Second)))
	buf := make([]byte, 5)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf))
	_, err = root.OpenTCP(context.Background(), transport.TCPRequest{ReverseToken: dataToken})
	require.Error(t, err, "TCP token is single-use")
}

func TestTCPUDPTokenWrongOwnerRejected(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	addr := tcpTestEndpoint(t, s, tlsConfig)
	token := s.currentConfig().Control.Token
	root := tcpTestDial(t, addr, "owner", "pw", token)
	_, ack := tcpTestRegister(t, root, "owner", token, config.Mapping{ID: "udp", Type: "udp", LocalAddr: "127.0.0.1:1", Enabled: true})
	udpToken := ack.Results[0].UDPToken
	owner := liveTCPTestSession(t, s, "owner")
	other := tcpTestDial(t, addr, "other", "pw", token)
	tcpTestRegister(t, other, "other", token)
	_, err := other.OpenUDP(context.Background(), udpToken)
	require.ErrorIs(t, err, transport.ErrAuthentication)
	_, err = s.mappings.udpRuntimeFor(udpToken, &clientSession{id: "owner", tcpPeer: owner.tcpPeer, transport: transport.TCP})
	require.Error(t, err)
	d, err := root.OpenUDP(context.Background(), udpToken)
	require.NoError(t, err)
	defer d.Close()
	public, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(t, err)
	defer public.Close()
	publicAddr := public.LocalAddr().String()
	require.NoError(t, d.Send([]byte("reply"), publicAddr))
	require.NoError(t, public.SetReadDeadline(time.Now().Add(time.Second)))
	buf := make([]byte, 100)
	n, _, err := public.ReadFromUDP(buf)
	require.NoError(t, err)
	require.Equal(t, "reply", string(buf[:n]))
	_, err = public.WriteToUDP([]byte("request"), &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: ack.Results[0].AssignedPort})
	require.NoError(t, err)
	data, peer, err := d.Receive()
	require.NoError(t, err)
	require.Equal(t, "request", string(data))
	require.Equal(t, publicAddr, peer)
	require.NoError(t, d.Send([]byte{0}, udpVirtualAddress(udpToken)))
	require.NoError(t, d.Send([]byte("after keepalive"), publicAddr))
	n, _, err = public.ReadFromUDP(buf)
	require.NoError(t, err, "virtual keepalive must be dropped without closing the bridge")
	require.Equal(t, "after keepalive", string(buf[:n]))
	require.NoError(t, d.Close())
	d2, err := root.OpenUDP(context.Background(), udpToken)
	require.NoError(t, err, "UDP token is reusable on live owner")
	defer d2.Close()
}

func TestTCPRevokeClosesOldRoot(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	root := tcpTestDial(t, tcpTestEndpoint(t, s, tlsConfig), "owner", "pw", s.currentConfig().Control.Token)
	c, _ := tcpTestRegister(t, root, "owner", s.currentConfig().Control.Token)
	owner := liveTCPTestSession(t, s, "owner")
	echo, stopEcho := testutil.StartTCPEcho(t)
	defer stopEcho()
	child, err := root.OpenTCP(context.Background(), transport.TCPRequest{Target: echo})
	require.NoError(t, err)
	defer child.Close()
	requireTCPEcho(t, child, "before revoke")
	_, _, err = s.RotateCredentials("new-password", "new-control-token-12345678", "manual")
	require.NoError(t, err)
	require.NoError(t, s.RevokePreviousCredentials())
	require.Eventually(t, func() bool { _, err := root.Probe(context.Background()); return err != nil }, time.Second, time.Millisecond)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = control.ReadMessage(c)
	require.Error(t, err)
	require.NoError(t, child.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = child.Read(make([]byte, 1))
	require.Error(t, err)
	require.False(t, isTimeout(err), "revocation must close active children as well as root")
	require.Eventually(t, func() bool {
		select {
		case <-owner.done:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
}

func TestTCPUserpassAuth(t *testing.T) {
	s, _ := tcpTestServer(t)
	s.settingsMu.Lock()
	next := cloneConfig(s.cfg)
	next.Auth = config.AuthConfig{Type: "userpass", Users: map[string]string{"alice": "secret"}}
	s.cfg = next
	s.settingsMu.Unlock()
	g := transport.Greeting{ClientID: "owner", Auth: "alice:secret", ControlToken: next.Control.Token}
	p, err := s.authenticateTCP(context.Background(), &net.TCPAddr{}, g)
	require.NoError(t, err)
	require.Equal(t, "owner", p.ClientID)
	require.Equal(t, sha256.Sum256([]byte(next.Control.Token)), p.TokenDigest)
	g.Auth = "alice:wrong"
	_, err = s.authenticateTCP(context.Background(), &net.TCPAddr{}, g)
	require.ErrorIs(t, err, transport.ErrAuthentication)
	_, _, err = s.RotateCredentials("new-password", "new-control-token-12345678", "manual")
	require.ErrorIs(t, err, ErrUnsupportedAuthType)
}

func TestTCPServerOptionsDirectionAndCurrentConfig(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	s.settingsMu.Lock()
	next := cloneConfig(s.cfg)
	next.Mode = config.ModeTCP
	next.Bandwidth.Up = config.Bandwidth(11111)
	next.Bandwidth.Down = config.Bandwidth(77777)
	s.cfg = next
	s.settingsMu.Unlock()
	opts := s.tcpServerOptions(tlsConfig)
	require.Equal(t, config.ModeTCP, opts.Mode)
	require.Equal(t, uint64(77777), opts.Limits.Up, "server receive is client Up")
	require.Equal(t, uint64(11111), opts.Limits.Down, "server transmit is client Down")
	next = cloneConfig(next)
	next.Mode = config.ModeHY2
	s.settingsMu.Lock()
	s.cfg = next
	s.settingsMu.Unlock()
	_, err := opts.Authenticate(context.Background(), &net.TCPAddr{}, transport.Greeting{ClientID: "owner", Auth: "pw", ControlToken: next.Control.Token})
	require.ErrorIs(t, err, transport.ErrAuthentication, "callback must not capture stale mode")
}

func TestTCPDataWrongOwnerOpenResultDoesNotConsume(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	addr := tcpTestEndpoint(t, s, tlsConfig)
	token := s.currentConfig().Control.Token
	root := tcpTestDial(t, addr, "owner", "pw", token)
	tcpTestRegister(t, root, "owner", token, config.Mapping{ID: "tcp", Type: "tcp", LocalAddr: "127.0.0.1:1", Enabled: true})
	owner := liveTCPTestSession(t, s, "owner")
	other := tcpTestDial(t, addr, "other", "pw", token)
	c, _ := tcpTestRegister(t, other, "other", token)
	s.mappings.mu.Lock()
	rt := owner.mappings["tcp"]
	s.mappings.mu.Unlock()
	a, b := net.Pipe()
	defer b.Close()
	dataToken, err := s.data.add(&pendingConn{conn: a, mapping: rt, owner: owner, expires: time.Now().Add(time.Second)})
	require.NoError(t, err)
	require.NoError(t, control.WriteMessage(c, &control.OpenResult{Type: control.TypeOpenResult, Token: dataToken, OK: false}))
	require.NoError(t, controlPingForTest(c)) // Barrier: OpenResult was processed.
	claimed, err := root.OpenTCP(context.Background(), transport.TCPRequest{ReverseToken: dataToken})
	require.NoError(t, err, "another control session cannot fail this owner's token")
	defer claimed.Close()
}

func TestTCPUDPWorkerRetargetClosesStreamKeepsToken(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	root := tcpTestDial(t, tcpTestEndpoint(t, s, tlsConfig), "owner", "pw", s.currentConfig().Control.Token)
	_, ack := tcpTestRegister(t, root, "owner", s.currentConfig().Control.Token, config.Mapping{ID: "udp", Type: "udp", LocalAddr: "127.0.0.1:1", Enabled: true})
	owner := liveTCPTestSession(t, s, "owner")
	d, err := root.OpenUDP(context.Background(), ack.Results[0].UDPToken)
	require.NoError(t, err)
	defer d.Close()
	// Prove the acknowledged stream has attached before changing its runtime.
	s.mappings.mu.Lock()
	rt := owner.mappings["udp"]
	s.mappings.mu.Unlock()
	require.Eventually(t, func() bool { rt.udpMu.Lock(); defer rt.udpMu.Unlock(); return rt.udpSession != nil }, time.Second, time.Millisecond)
	results, accepted, _ := s.mappings.update(owner, 1, []config.Mapping{{ID: "udp", Type: "udp", LocalAddr: "127.0.0.1:2", Enabled: true}})
	require.True(t, accepted)
	require.Equal(t, ack.Results[0].UDPToken, results[0].UDPToken)
	done := make(chan error, 1)
	go func() { _, _, err := d.Receive(); done <- err }()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("retarget left the old UDP stream alive")
	}
	d2, err := root.OpenUDP(context.Background(), results[0].UDPToken)
	require.NoError(t, err)
	defer d2.Close()
	require.NoError(t, s.removeMapping("owner", "udp"))
	go func() { _, _, err := d2.Receive(); done <- err }()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("removal left UDP stream alive")
	}
}

func TestTCPPeerRegistrationDeadlineDoesNotWaitForManager(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	accepted := make(chan struct{})
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		peer, err := transport.AcceptTCP(ctx, c, s.tcpServerOptions(tlsConfig))
		if err != nil {
			return
		}
		close(accepted)
		_ = s.serveTCPPeer(context.WithValue(ctx, tcpRegistrationDeadlineKey{}, time.Now().Add(100*time.Millisecond)), peer)
	}()
	root := tcpTestDial(t, l.Addr().String(), "owner", "pw", s.currentConfig().Control.Token)
	<-accepted
	s.mappings.mu.Lock()
	defer s.mappings.mu.Unlock()
	require.Eventually(t, func() bool { _, err := root.Probe(context.Background()); return err != nil }, 500*time.Millisecond, time.Millisecond, "registration timer must close root even while mapping registration holds manager lock")
}

func TestTCPRevokeCannotPromoteRootGeneration(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	root := tcpTestDial(t, tcpTestEndpoint(t, s, tlsConfig), "owner", "pw", s.currentConfig().Control.Token)
	tcpTestRegister(t, root, "owner", s.currentConfig().Control.Token)
	owner := liveTCPTestSession(t, s, "owner")
	_, _, err := s.RotateCredentials("new-password", "", "manual")
	require.NoError(t, err)
	owner.setCredentialStateFor(2, 2, true, true) // A reported/pushed update is not new root authentication.
	require.NoError(t, s.RevokePreviousCredentials())
	require.Eventually(t, func() bool { _, err := root.Probe(context.Background()); return err != nil }, time.Second, time.Millisecond)
}

func TestTCPPeerReconnectClosesPreviousRoot(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	addr := tcpTestEndpoint(t, s, tlsConfig)
	token := s.currentConfig().Control.Token
	root := tcpTestDial(t, addr, "owner", "pw", token)
	_, ack := tcpTestRegister(t, root, "owner", token, config.Mapping{ID: "udp", Type: "udp", LocalAddr: "127.0.0.1:1", Enabled: true})
	old := liveTCPTestSession(t, s, "owner")
	d, err := root.OpenUDP(context.Background(), ack.Results[0].UDPToken)
	require.NoError(t, err)
	defer d.Close()
	replacement := tcpTestDial(t, addr, "owner", "pw", token)
	_, nextAck := tcpTestRegister(t, replacement, "owner", token, config.Mapping{ID: "udp", Type: "udp", LocalAddr: "127.0.0.1:1", Enabled: true})
	current := liveTCPTestSession(t, s, "owner")
	require.NotSame(t, old, current)
	require.NotEqual(t, ack.Results[0].UDPToken, nextAck.Results[0].UDPToken)
	require.Eventually(t, func() bool { _, err := root.Probe(context.Background()); return err != nil }, time.Second, time.Millisecond)
	_, err = s.mappings.udpRuntimeFor(nextAck.Results[0].UDPToken, old)
	require.Error(t, err)
	d2, err := replacement.OpenUDP(context.Background(), nextAck.Results[0].UDPToken)
	require.NoError(t, err)
	defer d2.Close()
}

func TestTCPPeerProxyAndControlClose(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	root := tcpTestDial(t, tcpTestEndpoint(t, s, tlsConfig), "owner", "pw", s.currentConfig().Control.Token)
	c, _ := tcpTestRegister(t, root, "owner", s.currentConfig().Control.Token)
	addr, stop := testutil.StartTCPEcho(t)
	defer stop()
	proxy, err := root.OpenTCP(context.Background(), transport.TCPRequest{Target: addr})
	require.NoError(t, err)
	defer proxy.Close()
	requireTCPEcho(t, proxy, "hello")
	require.NoError(t, c.Close())
	require.Eventually(t, func() bool { _, err := root.Probe(context.Background()); return err != nil }, time.Second, time.Millisecond)
	require.NoError(t, proxy.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = proxy.Read(make([]byte, 1))
	require.Error(t, err)
	require.False(t, isTimeout(err))
}

func TestTCPPeerHeartbeatAndIdleTimeout(t *testing.T) {
	s, _ := tcpTestServer(t)
	a, b := net.Pipe()
	defer b.Close()
	owner := &clientSession{id: "heartbeat", conn: a, transport: transport.TCP, ready: make(chan struct{}), done: make(chan struct{})}
	s.mappings.register(owner, nil)
	owner.markReady()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer a.Close()
		s.control.loopWithHeartbeat(owner, 10*time.Millisecond, 50*time.Millisecond)
	}()
	require.NoError(t, b.SetDeadline(time.Now().Add(time.Second)))
	msg, err := control.ReadMessage(b)
	require.NoError(t, err)
	ping, ok := msg.(*control.Ping)
	require.True(t, ok, "server must send control heartbeats")
	require.NoError(t, control.WriteMessage(b, &control.Pong{Type: control.TypePong, TS: ping.TS}))
	// Continue reading heartbeats, but deliberately stop responding.
	for {
		_, err := control.ReadMessage(b)
		if err != nil {
			require.False(t, isTimeout(err))
			break
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("inactive session did not detach")
	}
	require.Empty(t, s.mappings.sessionSnapshot())
}

func TestTCPUDPBridgeQueueDropsWithoutClosingMapping(t *testing.T) {
	s, _ := tcpTestServer(t)
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(t, err)
	rt := &mappingRuntime{id: "udp", udpConn: udp, udpToken: "0123456789abcdef0123456789abcdef"}
	defer rt.close()
	a, b := net.Pipe()
	defer b.Close()
	d := transport.NewDatagramConn(a)
	defer d.Close()
	public := s.attachPublicUDPConn(rt, d)
	done := make(chan struct{})
	go func() { s.bridgeTCPUDP(rt, public, d); close(done) }()
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	require.NoError(t, err)
	defer peer.Close()
	// b never reads: one writer stalls and the bounded queue fills.
	for i := 0; i < 64; i++ {
		_, err := peer.WriteToUDP([]byte("packet"), udp.LocalAddr().(*net.UDPAddr))
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool { return rt.udpDrops.Load() > 0 }, time.Second, time.Millisecond)
	require.False(t, rt.closed.Load())
	require.NoError(t, transport.WriteDatagramFrame(b, []byte("still alive"), peer.LocalAddr().String()))
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))
	buf := make([]byte, 100)
	n, _, err := peer.ReadFromUDP(buf)
	require.NoError(t, err)
	require.Equal(t, "still alive", string(buf[:n]))
	require.NoError(t, b.Close())
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bridge did not release blocked queue/read workers")
	}
	rt.udpMu.Lock()
	require.Nil(t, rt.udpSession)
	rt.udpMu.Unlock()
}

func TestTCPPeerPreviousAuthWithUnchangedToken(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	addr := tcpTestEndpoint(t, s, tlsConfig)
	token := s.currentConfig().Control.Token
	_, _, err := s.RotateCredentials("new-password", "", "manual")
	require.NoError(t, err)
	root := tcpTestDial(t, addr, "previous", "pw", token)
	tcpTestRegister(t, root, "previous", token)
	owner := liveTCPTestSession(t, s, "previous")
	generation, _, _ := owner.authenticatedCredentialState()
	require.Equal(t, int64(1), generation, "previous password must retain previous generation even when token is unchanged")
	require.NoError(t, s.RevokePreviousCredentials())
	require.Eventually(t, func() bool { _, err := root.Probe(context.Background()); return err != nil }, time.Second, time.Millisecond)
}

func TestTCPPeerRegistersBeforeAndSurvivesBudget(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		peer, err := transport.AcceptTCP(ctx, c, s.tcpServerOptions(tlsConfig))
		if err != nil {
			return
		}
		_ = s.serveTCPPeer(context.WithValue(ctx, tcpRegistrationDeadlineKey{}, time.Now().Add(100*time.Millisecond)), peer)
	}()
	root := tcpTestDial(t, l.Addr().String(), "owner", "pw", s.currentConfig().Control.Token)
	c, _ := tcpTestRegister(t, root, "owner", s.currentConfig().Control.Token)
	time.Sleep(150 * time.Millisecond)
	require.NoError(t, c.SetDeadline(time.Now().Add(time.Second)))
	require.NoError(t, controlPingForTest(c), "registration deadline must not cancel established root")
}

func TestTCPPeerControlCloseCancelsPendingProxyDial(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	root := tcpTestDial(t, tcpTestEndpoint(t, s, tlsConfig), "owner", "pw", s.currentConfig().Control.Token)
	c, _ := tcpTestRegister(t, root, "owner", s.currentConfig().Control.Token)
	addr, stop := testutil.StartTCPEcho(t)
	defer stop()
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	original := outboundDialer
	// Delay the real dial at socket creation, rather than depending on a remote
	// blackhole address. Its context must belong to the root, not the endpoint.
	outboundDialer = net.Dialer{Timeout: 10 * time.Second, ControlContext: func(ctx context.Context, _, _ string, _ syscall.RawConn) error {
		close(started)
		select {
		case <-ctx.Done():
			close(canceled)
			return ctx.Err()
		case <-release:
			return net.ErrClosed
		}
	}}
	defer func() { close(release); outboundDialer = original }()
	openDone := make(chan struct{})
	go func() {
		defer close(openDone)
		child, _ := root.OpenTCP(context.Background(), transport.TCPRequest{Target: addr})
		if child != nil {
			_ = child.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("proxy dial did not start")
	}
	require.NoError(t, c.Close())
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("root closure did not cancel its pending proxy dial")
	}
	select {
	case <-openDone:
	case <-time.After(time.Second):
		t.Fatal("root closure did not release pending client open")
	}
}
