package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/testutil"
	"hy2tun/internal/transport"
)

// Catch mode branches that bind an unwanted protocol or choose two :0 ports.
func TestServerModeListenerMatrix(t *testing.T) {
	for _, tc := range []struct {
		mode config.TransportMode
		want []transport.Kind
	}{
		{config.ModeHY2, []transport.Kind{transport.HY2}},
		{config.ModeTCP, []transport.Kind{transport.TCP}},
		{config.ModeAuto, []transport.Kind{transport.HY2, transport.TCP}},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			cfg := listenerTestConfig(t, tc.mode)
			s, err := New(cfg, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			addresses := s.ListenAddresses()
			require.Len(t, addresses, len(tc.want))
			for _, kind := range tc.want {
				require.NotEmpty(t, addresses[kind])
				_, port, err := net.SplitHostPort(addresses[kind])
				require.NoError(t, err)
				require.NotEqual(t, "0", port)
				_, legacyPort, err := net.SplitHostPort(s.ListenAddr().String())
				require.NoError(t, err)
				require.Equal(t, legacyPort, port)
			}
			// The reported sockets really own their respective protocol ports.
			for _, kind := range tc.want {
				if kind == transport.HY2 {
					c, err := net.ListenPacket("udp", addresses[kind])
					if c != nil {
						_ = c.Close()
					}
					require.Error(t, err)
				} else {
					l, err := net.Listen("tcp", addresses[kind])
					if l != nil {
						_ = l.Close()
					}
					require.Error(t, err)
				}
			}
			delete(addresses, tc.want[0])
			require.Len(t, s.ListenAddresses(), len(tc.want), "snapshot must not mutate endpoints")
		})
	}
}

func listenerTestConfig(t *testing.T, mode config.TransportMode) *config.ServerConfig {
	t.Helper()
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	cfg.Mode = mode
	cfg.ListenHost = "127.0.0.1"
	return cfg
}

// Catch returning partial auto success and leaking the first UDP binding.
func TestDualStartupTCPBindFailureCleansUDP(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	cfg := listenerTestConfig(t, config.ModeAuto)
	cfg.Listen = occupied.Addr().String()
	s, err := New(cfg, nil)
	if s != nil {
		_ = s.Close()
	}
	require.ErrorContains(t, err, "TCP")
	require.Nil(t, s)
	udp, err := net.ListenPacket("udp", cfg.Listen)
	require.NoError(t, err, "failed second binding leaked UDP")
	defer udp.Close()
}

// Catch random auto :0 failures when UDP's selected port is occupied in TCP.
// Start at a real conflicting pair, then verify bounded ephemeral reselection
// releases the failed UDP socket and leaves both protocols on one usable port.
func TestDualStartupEphemeralTCPConflictRetriesPair(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	s, err := New(listenerTestConfig(t, config.ModeTCP), nil)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.endpoints[transport.TCP].Close())
	s.endpoints = make(map[transport.Kind]endpoint)
	cfg := listenerTestConfig(t, config.ModeAuto)
	// The initial candidate is deliberately occupied; cfg.Listen remains :0.
	require.NoError(t, s.prepareInitialEndpoints(cfg, occupied.Addr().String()))
	addresses := s.ListenAddresses()
	require.Len(t, addresses, 2)
	require.Equal(t, addresses[transport.HY2], addresses[transport.TCP])
	require.NotEqual(t, occupied.Addr().String(), addresses[transport.TCP])
	udp, err := net.ListenPacket("udp", occupied.Addr().String())
	require.NoError(t, err, "retry leaked its failed UDP candidate")
	defer udp.Close()
}

// Catch constructing UDP/HY2/firewall dependencies for pure TCP. Invalid HY2
// obfs configuration must be irrelevant, and the same UDP port stays available.
func TestTCPModeDoesNotConstructHY2(t *testing.T) {
	cfg := listenerTestConfig(t, config.ModeTCP)
	cfg.Obfs.Type = "not-a-HY2-obfuscator"
	s, err := New(cfg, nil)
	require.NoError(t, err)
	defer s.Close()
	require.Nil(t, s.hyServer)
	require.Nil(t, s.packetConn)
	udp, err := net.ListenPacket("udp", s.ListenAddr().String())
	require.NoError(t, err)
	defer udp.Close()
}

// Catch reserving only the listen protocol instead of its numeric tunnel port.
func TestTunnelPortReservedAcrossTransports(t *testing.T) {
	for _, mode := range []config.TransportMode{config.ModeHY2, config.ModeTCP, config.ModeAuto} {
		t.Run(string(mode), func(t *testing.T) {
			s, err := New(listenerTestConfig(t, mode), nil)
			require.NoError(t, err)
			defer s.Close()
			port := addrPort(s.ListenAddr())
			sess := &clientSession{id: "reserved"}
			results := s.mappings.register(sess, []config.Mapping{
				{ID: "tcp", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:1", Enabled: true},
				{ID: "udp", Type: "udp", RemotePort: port, LocalAddr: "127.0.0.1:1", Enabled: true},
			})
			require.Len(t, results, 2)
			for _, result := range results {
				require.False(t, result.OK)
				require.Equal(t, string(control.CodePortReserved), result.Error)
			}
		})
	}
}

// Catch launching a handshake goroutine before the 64-slot admission decision.
func TestTCPHandshakeAdmissionBounded(t *testing.T) {
	s, _ := startServer(t, listenerTestConfig(t, config.ModeTCP))
	addr := s.ListenAddr().String()
	baseline := runtime.NumGoroutine()
	conns := make([]net.Conn, 0, 64)
	for i := 0; i < 64; i++ {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		require.NoError(t, err)
		conns = append(conns, c)
		t.Cleanup(func() { _ = c.Close() })
	}
	// Keep these 64 raw peers silent: they remain pending authentication.
	for i := 0; i < 128; i++ {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		require.NoError(t, err)
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, err = c.Read(make([]byte, 1))
		_ = c.Close()
		require.Error(t, err)
		if nerr, ok := err.(net.Error); ok {
			require.False(t, nerr.Timeout(), "excess connection not closed")
		}
	}
	require.Less(t, runtime.NumGoroutine()-baseline, 90, "excess peers created unbounded workers")
	_ = conns[0].Close()
	// An accepted slot can finish TLS+auth+control after a prior slot is released.
	var root transport.Session
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var err error
		root, err = transport.DialTCP(ctx, transport.DialOptions{TCPAddress: addr, TLS: &tls.Config{InsecureSkipVerify: true}, Auth: "pw", ControlToken: s.currentConfig().Control.Token, ClientID: "new"})
		return err == nil
	}, 2*time.Second, 10*time.Millisecond)
	defer root.Close()
	c, _ := tcpTestRegister(t, root, "new", s.currentConfig().Control.Token)
	requireControlPong(t, c)
}

func requireControlPong(t *testing.T, c net.Conn) {
	t.Helper()
	require.NoError(t, c.SetDeadline(time.Now().Add(time.Second)))
	require.NoError(t, control.WriteMessage(c, &control.Ping{Type: control.TypePing, TS: 42}))
	msg, err := control.ReadMessage(c)
	require.NoError(t, err)
	require.Equal(t, &control.Pong{Type: control.TypePong, TS: 42}, msg)
	require.NoError(t, c.SetDeadline(time.Time{}))
}

// Catch counting only registered controls: roots without Hello also count.
func TestTCPAuthenticatedRootAdmissionBounded(t *testing.T) {
	s, _ := startServer(t, listenerTestConfig(t, config.ModeTCP))
	addr, token := s.ListenAddr().String(), s.currentConfig().Control.Token
	roots := make([]transport.Session, 0, 256)
	for i := 0; i < 256; i++ {
		roots = append(roots, tcpTestDial(t, addr, fmt.Sprintf("root-%d", i), "pw", token))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	extra, err := transport.DialTCP(ctx, transport.DialOptions{TCPAddress: addr, TLS: &tls.Config{InsecureSkipVerify: true}, Auth: "pw", ControlToken: token, ClientID: "extra"})
	if extra != nil {
		_ = extra.Close()
	}
	require.Error(t, err, "257th root admitted without a control stream")
	require.Empty(t, s.Clients())
	_ = roots[0].Close()
	var next transport.Session
	require.Eventually(t, func() bool {
		var err error
		next, err = transport.DialTCP(ctx, transport.DialOptions{TCPAddress: addr, TLS: &tls.Config{InsecureSkipVerify: true}, Auth: "pw", ControlToken: token, ClientID: "next"})
		return err == nil
	}, time.Second, time.Millisecond)
	defer next.Close()
	tcpTestRegister(t, next, "next", token)
}

// Catch closing the whole server/Web or leaving unauthenticated roots behind.
func TestEndpointCloseIsTransportScoped(t *testing.T) {
	cfg := listenerTestConfig(t, config.ModeAuto)
	cfg.Web.Listen = "127.0.0.1:0"
	_, _, err := config.EnsureWebPassword(&cfg.Web)
	require.NoError(t, err)
	s, _ := startServer(t, cfg)
	root := tcpTestDial(t, s.ListenAddresses()[transport.TCP], "tcp", "pw", cfg.Control.Token)
	c, _ := tcpTestRegister(t, root, "tcp", cfg.Control.Token)
	owner := liveTCPTestSession(t, s, "tcp")
	noHello := tcpTestDial(t, s.ListenAddresses()[transport.TCP], "no-hello", "pw", cfg.Control.Token)
	raw, err := net.Dial("tcp", s.ListenAddresses()[transport.TCP])
	require.NoError(t, err)
	defer raw.Close()
	hy := newHY2Client(t, s, "pw", nil)
	hyControl, err := hy.TCP("_hy2tun_control_:0")
	require.NoError(t, err)
	defer hyControl.Close()
	require.NoError(t, control.WriteMessage(hyControl, &control.Hello{Type: control.TypeHello, Protocol: control.ProtocolVersion, ClientID: "hy2", Token: cfg.Control.Token}))
	_, err = control.ReadMessage(hyControl)
	require.NoError(t, err)
	s.listenersMu.Lock()
	ep := s.endpoints[transport.TCP]
	s.listenersMu.Unlock()
	require.NoError(t, ep.Close())
	require.Eventually(t, func() bool { return len(s.ListenAddresses()) == 1 && s.mappings.clientCount() == 1 }, 2*time.Second, time.Millisecond)
	select {
	case <-owner.done:
	case <-time.After(time.Second):
		t.Fatal("TCP owner not detached")
	}
	requireControlPong(t, hyControl)
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, err = control.ReadMessage(c)
	require.Error(t, err)
	_ = raw.SetReadDeadline(time.Now().Add(time.Second))
	_, err = io.ReadFull(raw, make([]byte, 1))
	require.Error(t, err)
	if nerr, ok := err.(net.Error); ok {
		require.False(t, nerr.Timeout())
	}
	require.Eventually(t, func() bool { _, err := noHello.Probe(context.Background()); return err != nil }, time.Second, time.Millisecond)
	require.False(t, s.closed.Load())
	require.Eventually(t, func() bool {
		web, err := net.DialTimeout("tcp", s.WebAddr(), time.Second)
		if web != nil {
			_ = web.Close()
		}
		return err == nil
	}, time.Second, time.Millisecond)
}

// Catch returning a stale UDP legacy address after its endpoint is retired.
func TestEndpointHY2RetirementKeepsTCP(t *testing.T) {
	s, _ := startServer(t, listenerTestConfig(t, config.ModeAuto))
	root := tcpTestDial(t, s.ListenAddresses()[transport.TCP], "tcp", "pw", s.currentConfig().Control.Token)
	c, _ := tcpTestRegister(t, root, "tcp", s.currentConfig().Control.Token)
	s.listenersMu.Lock()
	ep := s.endpoints[transport.HY2]
	s.listenersMu.Unlock()
	require.NoError(t, ep.Close())
	require.Eventually(t, func() bool { return len(s.ListenAddresses()) == 1 }, time.Second, time.Millisecond)
	require.Equal(t, "tcp", s.ListenAddr().Network())
	require.Equal(t, s.ListenAddresses()[transport.TCP], s.ListenAddr().String())
	requireControlPong(t, c)
	require.False(t, s.closed.Load())
}

// Catch an old endpoint's delayed Serve return detaching a newer endpoint's
// owners. This is lifecycle identity wiring, not online UpdateMode behavior.
func TestEndpointStaleRetirementPreservesReplacement(t *testing.T) {
	s, err := New(listenerTestConfig(t, config.ModeTCP), nil)
	require.NoError(t, err)
	defer s.Close()
	old := s.endpoints[transport.TCP]
	require.NoError(t, old.Close())
	next, err := s.prepareEndpoint(transport.TCP, s.currentConfig(), s.baseBindAddr)
	require.NoError(t, err)
	s.endpoints[transport.TCP] = next
	owner := &clientSession{id: "replacement", transport: transport.TCP, done: make(chan struct{})}
	s.mappings.register(owner, nil)
	s.retireEndpoint(old)
	require.Contains(t, s.ListenAddresses(), transport.TCP)
	require.Equal(t, 1, s.mappings.clientCount())
	select {
	case <-owner.done:
		t.Fatal("stale endpoint detached replacement")
	default:
	}
}

// Catch extending the raw-accept budget when a handshake worker is delayed.
func TestTCPAcceptedDeadlineIsNotRestarted(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	a, b := net.Pipe()
	defer b.Close()
	ctx := context.WithValue(context.Background(), tcpRegistrationDeadlineKey{}, time.Now().Add(-time.Second))
	err := s.serveTCPConn(ctx, a, tlsConfig)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// Catch a raw socket surviving its accepted deadline while a settings writer
// delays the per-handshake snapshot. The timeout must not wait for that lock.
func TestTCPAcceptedDeadlineClosesRawWhileSettingsBlocked(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	a, b := net.Pipe()
	defer b.Close()
	deadline := time.Now().Add(50 * time.Millisecond)
	ctx := context.WithValue(context.Background(), tcpRegistrationDeadlineKey{}, deadline)
	s.settingsMu.Lock()
	errors := make(chan error, 1)
	go func() { errors <- s.serveTCPConn(ctx, a, tlsConfig) }()
	_ = b.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, err := b.Read(make([]byte, 1))
	s.settingsMu.Unlock() // Always release the gate before any failing assertion.
	require.ErrorIs(t, err, io.EOF, "registration budget waited for settings lock")
	select {
	case err := <-errors:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("handshake did not unwind after settings release")
	}
}

// Catch accidentally caching the advertised mode at endpoint preparation.
func TestTCPEndpointUsesLiveHandshakeOptions(t *testing.T) {
	s, _ := startServer(t, listenerTestConfig(t, config.ModeAuto))
	s.settingsMu.Lock()
	next := cloneConfig(s.cfg)
	next.Mode = config.ModeTCP
	s.cfg = next
	s.settingsMu.Unlock()
	root := tcpTestDial(t, s.ListenAddresses()[transport.TCP], "live", "pw", s.currentConfig().Control.Token)
	info, err := root.Probe(context.Background())
	require.NoError(t, err)
	require.Equal(t, config.ModeTCP, info.Mode)
	// Mode changed only in-memory for the handshake check; no UpdateMode path.
	tcpTestRegister(t, root, "live", s.currentConfig().Control.Token)
}

// Catch overflow if admission is split into hasClient/clientCount operations.
func TestControlAdmissionCapacityCommittedUnderManager(t *testing.T) {
	s, err := New(listenerTestConfig(t, config.ModeHY2), nil)
	require.NoError(t, err)
	defer s.Close()
	for i := 0; i < 255; i++ {
		s.mappings.register(&clientSession{id: fmt.Sprintf("client-%d", i)}, nil)
	}
	start := make(chan struct{})
	errors := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() {
			<-start
			id := fmt.Sprintf("competing-%d", i)
			_, err := s.registerControl(&clientSession{id: id}, &control.Hello{ClientID: id, Token: s.currentConfig().Control.Token}, sessionMeta{Kind: transport.HY2}, time.Now().Add(time.Second), 1, false)
			errors <- err
		}()
	}
	close(start)
	admitted := 0
	for i := 0; i < 16; i++ {
		if <-errors == nil {
			admitted++
		}
	}
	require.Equal(t, 1, admitted)
	require.Equal(t, 256, s.mappings.clientCount())
	_, err = s.registerControl(&clientSession{id: "client-0"}, &control.Hello{ClientID: "client-0", Token: s.currentConfig().Control.Token}, sessionMeta{Kind: transport.HY2}, time.Now().Add(time.Second), 1, false)
	require.NoError(t, err, "same-ID reconnect does not consume another client slot")
}

// Catch a HY2 Hello already read before shutdown replacing a healthy TCP owner
// after the HY2 endpoint has been closed. The admission path and root are real.
func TestEndpointClosedHY2AdmissionPreservesTCP(t *testing.T) {
	s, _ := startServer(t, listenerTestConfig(t, config.ModeAuto))
	root := tcpTestDial(t, s.ListenAddresses()[transport.TCP], "healthy", "pw", s.currentConfig().Control.Token)
	c, _ := tcpTestRegister(t, root, "healthy", s.currentConfig().Control.Token)
	owner := liveTCPTestSession(t, s, "healthy")
	s.listenersMu.Lock()
	hy := s.endpoints[transport.HY2].(*hy2Endpoint)
	s.listenersMu.Unlock()
	require.NoError(t, hy.Close())
	hello := &control.Hello{ClientID: "healthy", Token: s.currentConfig().Control.Token}
	_, err := s.registerControl(&clientSession{id: "healthy", transport: transport.HY2}, hello, sessionMeta{Kind: transport.HY2, endpointClosed: &hy.closed}, time.Now().Add(time.Second), 1, false)
	require.Error(t, err)
	s.mappings.mu.Lock()
	actual := s.mappings.sessions["healthy"]
	s.mappings.mu.Unlock()
	require.Same(t, owner, actual)
	requireControlPong(t, c)
}

// Catch serving TLS/auth during New rather than waiting for Start.
func TestTCPEndpointPreparedDoesNotAcceptApplicationTraffic(t *testing.T) {
	s, err := New(listenerTestConfig(t, config.ModeTCP), nil)
	require.NoError(t, err)
	defer s.Close()
	raw, err := net.Dial("tcp", s.ListenAddr().String())
	require.NoError(t, err)
	conn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err = conn.HandshakeContext(ctx)
	_ = conn.Close()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	go func() { _ = s.Start(context.Background()) }()
	root := tcpTestDial(t, s.ListenAddr().String(), "started", "pw", s.currentConfig().Control.Token)
	tcpTestRegister(t, root, "started", s.currentConfig().Control.Token)
}

// Catch claiming complete auto startup when a required Serve cannot run.
func TestDualStartRequiredEndpointFailureReturnsError(t *testing.T) {
	s, err := New(listenerTestConfig(t, config.ModeAuto), nil)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.endpoints[transport.TCP].Close())
	errors := make(chan error, 1)
	go func() { errors <- s.Start(context.Background()) }()
	select {
	case err := <-errors:
		require.ErrorContains(t, err, "serve tcp")
	case <-time.After(time.Second):
		t.Fatal("Start hid a required listener failure")
	}
	require.False(t, s.closed.Load(), "single-endpoint cleanup must not close shared Server")
	require.Equal(t, map[transport.Kind]string{transport.HY2: s.ListenAddr().String()}, s.ListenAddresses())
}

// Catch endpoint cleanup that leaves a pending public reverse connection alive
// or accidentally closes the other transport's listener/runtime.
func TestDetachTransportClosesOnlyOwnedMappings(t *testing.T) {
	s, err := New(listenerTestConfig(t, config.ModeAuto), nil)
	require.NoError(t, err)
	defer s.Close()
	tcpOwner := &clientSession{id: "tcp-owner", transport: transport.TCP, done: make(chan struct{})}
	hyOwner := &clientSession{id: "hy-owner", transport: transport.HY2, done: make(chan struct{})}
	for _, owner := range []*clientSession{tcpOwner, hyOwner} {
		results := s.mappings.register(owner, []config.Mapping{{ID: "mapping", Type: "tcp", LocalAddr: "127.0.0.1:1", Enabled: true}})
		require.True(t, results[0].OK)
	}
	owned := tcpOwner.mappings["mapping"]
	other := hyOwner.mappings["mapping"]
	a, b := net.Pipe()
	defer b.Close()
	_, err = s.data.add(&pendingConn{conn: a, mapping: owned, owner: tcpOwner, expires: time.Now().Add(time.Second)})
	require.NoError(t, err)
	s.mappings.detachTransport(transport.TCP)
	require.True(t, owned.closed.Load())
	require.False(t, other.closed.Load())
	require.Equal(t, 1, s.mappings.clientCount())
	_ = b.SetReadDeadline(time.Now().Add(time.Second))
	_, err = b.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
	c, err := net.DialTimeout("tcp", other.listeners.TCP[0].Addr().String(), time.Second)
	require.NoError(t, err)
	_ = c.Close()
	s.mappings.detachTransport(transport.TCP) // Repeat must not touch HY2.
	require.False(t, other.closed.Load())
}
