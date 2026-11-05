package server

import (
	"crypto/tls"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/transport"
)

func applyServerMode(s *Server, mode config.TransportMode) error {
	u, ok := any(s).(interface {
		UpdateMode(config.TransportMode) error
	})
	if !ok {
		return errors.New("mode updates are not implemented")
	}
	return u.UpdateMode(mode)
}

func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

// freeTCPAndUDPAddr returns a loopback address currently free for both
// transports. Callers that bind both numeric sides still retry on EADDRINUSE,
// closing the remaining race with another package's ephemeral listener.
func freeTCPAndUDPAddr(t *testing.T) string {
	t.Helper()

	for attempt := 0; attempt < 50; attempt++ {
		tcp, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := tcp.Addr().String()
		udp, err := net.ListenPacket("udp", addr)
		if err != nil {
			require.NoError(t, tcp.Close())
			continue
		}
		require.NoError(t, tcp.Close())
		require.NoError(t, udp.Close())
		return addr
	}
	t.Fatal("could not reserve a TCP/UDP port pair")
	return ""
}

// newServerOnFreePair constructs a server whose base numeric port is free for
// both TCP and UDP at selection time.
func newServerOnFreePair(t *testing.T, mode config.TransportMode, configure func(*config.ServerConfig)) (*Server, string) {
	t.Helper()

	for attempt := 0; attempt < 20; attempt++ {
		cfg := listenerTestConfig(t, mode)
		addr := freeTCPAndUDPAddr(t)
		cfg.Listen = addr
		if configure != nil {
			configure(cfg)
		}
		s, err := New(cfg, nil)
		if err == nil {
			return s, addr
		}
		if isAddrInUse(err) {
			continue
		}
		require.NoError(t, err)
	}
	t.Fatal("could not construct a server on a free TCP/UDP pair")
	return nil, ""
}

func TestServerModeBindConflictRollback(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		// Keep the TCP side occupied before starting the HY2 server so the
		// intended bind conflict is deterministic on both transports.
		addr := freeTCPAndUDPAddr(t)
		occupied, err := net.Listen("tcp", addr)
		require.NoError(t, err)
		cfg := listenerTestConfig(t, config.ModeHY2)
		cfg.Listen = addr
		s, err := New(cfg, nil)
		if err != nil {
			require.NoError(t, occupied.Close())
			if isAddrInUse(err) {
				continue
			}
			require.NoError(t, err)
		}
		stop, _, _ := startExistingServer(t, s)
		require.ErrorContains(t, applyServerMode(s, config.ModeAuto), "TCP")
		require.Equal(t, config.ModeHY2, s.currentConfig().Mode)
		require.Equal(t, map[transport.Kind]string{transport.HY2: addr}, s.ListenAddresses())
		hy := newHY2Client(t, s, "pw", nil)
		c, err := hy.TCP("_hy2tun_control_:0")
		require.NoError(t, err)
		require.NoError(t, control.WriteMessage(c, &control.Hello{Type: control.TypeHello, Protocol: control.ProtocolVersion, ClientID: "hy", Token: s.currentConfig().Control.Token}))
		ack, err := control.ReadMessage(c)
		require.NoError(t, err)
		require.True(t, ack.(*control.HelloAck).OK)
		requireControlPong(t, c)
		require.NoError(t, c.Close())
		stop()
		require.NoError(t, occupied.Close())
		return
	}
	t.Fatal("could not bind the HY2 server around an occupied TCP port")
}

func TestServerModeSaveFailureClosesNewListener(t *testing.T) {
	s, addr := newServerOnFreePair(t, config.ModeHY2, func(cfg *config.ServerConfig) {
		cfg.ConfigPath = filepath.Join(t.TempDir(), "server.yaml")
	})
	stop, startErr, startDone := startExistingServer(t, s)
	defer stop()

	failure := errors.New("disk unavailable")
	s.saveConfig = func(_ string, cfg *config.ServerConfig) error {
		require.Equal(t, config.ModeAuto, cfg.Mode)
		// No candidate may be in the Serve set before persistence. This is the
		// direct candidate-state check; the ClientHello below proves the bound
		// socket does not answer either.
		s.listenersMu.Lock()
		servedTCP := false
		for ep := range s.serving {
			if ep.Kind() == transport.TCP {
				servedTCP = true
			}
		}
		s.listenersMu.Unlock()
		require.False(t, servedTCP, "candidate TCP endpoint was served before persistence")
		c, err := net.DialTimeout("tcp", addr, time.Second)
		require.NoError(t, err)
		defer c.Close()
		require.NoError(t, c.SetDeadline(time.Now().Add(250*time.Millisecond)))
		tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true})
		err = tc.Handshake()
		require.Error(t, err, "candidate TCP endpoint was served before persistence")
		var netErr net.Error
		require.ErrorAs(t, err, &netErr)
		require.True(t, netErr.Timeout(), "candidate TCP endpoint responded before persistence: %v", err)
		return failure
	}

	deadline := time.Now().Add(2 * time.Second)
	var modeErr error
	for {
		modeErr = applyServerMode(s, config.ModeAuto)
		if modeErr == nil || !isAddrInUse(modeErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mode update kept hitting a port conflict: %v", modeErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.ErrorIs(t, modeErr, failure)
	require.Equal(t, config.ModeHY2, s.currentConfig().Mode)

	l, err := net.Listen("tcp", addr)
	require.NoError(t, err, "failed persist leaked candidate socket")
	defer l.Close()

	select {
	case <-startDone:
		t.Fatalf("Start returned after failed mode persist: %v", <-startErr)
	case <-time.After(250 * time.Millisecond):
	}

	// The old HY2 endpoint must still serve after the rollback.
	hy := newHY2Client(t, s, "pw", nil)
	c, err := hy.TCP("_hy2tun_control_:0")
	require.NoError(t, err)
	defer c.Close()
	require.NoError(t, control.WriteMessage(c, &control.Hello{Type: control.TypeHello, Protocol: control.ProtocolVersion, ClientID: "hy", Token: s.currentConfig().Control.Token}))
	ack, err := control.ReadMessage(c)
	require.NoError(t, err)
	require.True(t, ack.(*control.HelloAck).OK)
	requireControlPong(t, c)
}

func TestServerDisablingHY2LeavesTCPAndWeb(t *testing.T) {
	var s *Server
	for attempt := 0; attempt < 20; attempt++ {
		cfg := listenerTestConfig(t, config.ModeAuto)
		cfg.Listen = freeTCPAndUDPAddr(t)
		cfg.Web.Listen = "127.0.0.1:0"
		_, _, err := config.EnsureWebPassword(&cfg.Web)
		require.NoError(t, err)
		s, err = New(cfg, nil)
		if err == nil {
			break
		}
		if !isAddrInUse(err) {
			require.NoError(t, err)
		}
		s = nil
	}
	require.NotNil(t, s, "could not start the dual-mode server")
	stop, startErr, startDone := startExistingServer(t, s)
	defer stop()

	root := tcpTestDial(t, s.ListenAddresses()[transport.TCP], "tcp", "pw", s.currentConfig().Control.Token)
	c, _ := tcpTestRegister(t, root, "tcp", s.currentConfig().Control.Token)
	tcpView, ok := s.Client("tcp")
	require.True(t, ok)
	require.Equal(t, "tcp", tcpView.Transport)
	hy := newHY2Client(t, s, "pw", nil)
	hyControl, err := hy.TCP("_hy2tun_control_:0")
	require.NoError(t, err)
	defer hyControl.Close()
	require.NoError(t, control.WriteMessage(hyControl, &control.Hello{Type: control.TypeHello, Protocol: control.ProtocolVersion, ClientID: "hy", Token: s.currentConfig().Control.Token}))
	_, err = control.ReadMessage(hyControl)
	require.NoError(t, err)
	hyView, ok := s.Client("hy")
	require.True(t, ok)
	require.Equal(t, "hy2", hyView.Transport)
	web := s.WebAddr()

	s.listenersMu.Lock()
	oldHY2 := s.endpoints[transport.HY2]
	s.listenersMu.Unlock()
	require.NotNil(t, oldHY2)
	require.NoError(t, applyServerMode(s, config.ModeTCP))
	require.Equal(t, config.ModeTCP, s.Mode())
	require.Equal(t, config.ModeTCP, s.Settings().Mode)
	require.Equal(t, config.ModeTCP, s.ConfigSummary().Mode)
	select {
	case <-oldHY2.workerDone():
	case <-time.After(2 * time.Second):
		t.Fatal("retired HY2 Serve worker did not finish")
	}
	select {
	case <-startDone:
		t.Fatalf("server Start returned after intentional HY2 retirement: %v", <-startErr)
	case <-time.After(250 * time.Millisecond):
	}
	requireControlPong(t, c)
	require.False(t, s.closed.Load())
	require.Equal(t, web, s.WebAddr())
	webConn, err := net.DialTimeout("tcp", web, time.Second)
	require.NoError(t, err)
	defer webConn.Close()
	require.Eventually(t, func() bool { v, ok := s.Client("hy"); return ok && !v.Online }, time.Second, time.Millisecond)
	offline, ok := s.Client("hy")
	require.True(t, ok)
	require.Equal(t, "hy2", offline.Transport, "last actual transport is diagnostic only")
	require.NoError(t, hyControl.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = control.ReadMessage(hyControl)
	require.Error(t, err)

	deadline := time.Now().Add(2 * time.Second)
	var modeErr error
	for {
		modeErr = applyServerMode(s, config.ModeAuto)
		if modeErr == nil || !isAddrInUse(modeErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HY2 re-enable kept hitting a port conflict: %v", modeErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.NoError(t, modeErr, "intentional HY2 retirement must not terminate Start")
	require.Equal(t, config.ModeAuto, s.Mode())
	addrs := s.ListenAddresses()
	require.Contains(t, addrs, transport.HY2)
	require.Contains(t, addrs, transport.TCP)
	s.listenersMu.Lock()
	newHY2 := s.endpoints[transport.HY2]
	s.listenersMu.Unlock()
	require.NotNil(t, newHY2)
	select {
	case <-newHY2.ready():
	case <-time.After(2 * time.Second):
		t.Fatal("re-enabled HY2 endpoint did not start serving")
	}
	// Re-enable must actually serve, not merely construct a client.
	reEnabled := newHY2Client(t, s, "pw", nil)
	reControl, err := reEnabled.TCP("_hy2tun_control_:0")
	require.NoError(t, err)
	defer reControl.Close()
	require.NoError(t, control.WriteMessage(reControl, &control.Hello{Type: control.TypeHello, Protocol: control.ProtocolVersion, ClientID: "hy2", Token: s.currentConfig().Control.Token}))
	ack, err := control.ReadMessage(reControl)
	require.NoError(t, err)
	require.True(t, ack.(*control.HelloAck).OK)
	requireControlPong(t, reControl)
	select {
	case <-startDone:
		t.Fatalf("server Start returned after HY2 re-enable: %v", <-startErr)
	case <-time.After(250 * time.Millisecond):
	}
	requireControlPong(t, c)
}

// Close must serialize with an in-flight mode transaction: whichever wins, no
// candidate listener survives and credentials are not overwritten.
func TestServerModeCloseWinsCandidateCleanup(t *testing.T) {
	for range 8 {
		s, addr := newServerOnFreePair(t, config.ModeTCP, nil)
		generation := s.currentConfig().Credentials.Generation
		entered := make(chan struct{})
		release := make(chan struct{})
		s.modeEnterHook = func() {
			close(entered)
			<-release
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var modeErr, closeErr error
		go func() {
			defer wg.Done()
			<-start
			modeErr = applyServerMode(s, config.ModeAuto)
		}()
		go func() {
			defer wg.Done()
			<-start
			closeErr = s.Close()
		}()
		close(start)
		// The barrier proves the mode goroutine entered the transaction before
		// Close can win; without it the test can pass without exercising it.
		<-entered
		close(release)
		wg.Wait()
		require.NoError(t, closeErr)
		require.Empty(t, s.ListenAddresses())
		require.Equal(t, generation, s.currentConfig().Credentials.Generation)
		if modeErr != nil {
			if isAddrInUse(modeErr) {
				continue
			}
			require.ErrorContains(t, modeErr, "closed")
		}
		require.Eventually(t, func() bool {
			udp, err := net.ListenPacket("udp", addr)
			if err != nil {
				return false
			}
			udp.Close()
			return true
		}, 2*time.Second, 10*time.Millisecond, "mode transaction leaked UDP candidate")
		require.Eventually(t, func() bool {
			tcp, err := net.Listen("tcp", addr)
			if err != nil {
				return false
			}
			tcp.Close()
			return true
		}, 2*time.Second, 10*time.Millisecond, "mode transaction leaked TCP candidate")
	}
}

func TestModeUpdateRaceWithRotationAndClose(t *testing.T) {
	for range 8 {
		s, addr := newServerOnFreePair(t, config.ModeTCP, nil)
		var wg sync.WaitGroup
		wg.Add(2)
		var modeErr, rotateErr error
		go func() {
			defer wg.Done()
			modeErr = applyServerMode(s, config.ModeAuto)
		}()
		go func() {
			defer wg.Done()
			_, _, rotateErr = s.RotateCredentials("rotated-password", "rotated-control-token", "manual")
		}()
		wg.Wait()
		require.NoError(t, rotateErr)
		if isAddrInUse(modeErr) {
			require.NoError(t, s.Close())
			continue
		}
		require.NoError(t, modeErr)
		require.Equal(t, "rotated-password", s.currentConfig().Auth.Password)
		require.Equal(t, int64(2), s.currentConfig().Credentials.Generation)
		require.Equal(t, config.ModeAuto, s.currentConfig().Mode)
		require.NoError(t, s.Close())
		require.Error(t, applyServerMode(s, config.ModeHY2))
		require.Empty(t, s.ListenAddresses())
		udp, err := net.ListenPacket("udp", addr)
		require.NoError(t, err)
		udp.Close()
		tcp, err := net.Listen("tcp", addr)
		require.NoError(t, err)
		tcp.Close()
	}
}
