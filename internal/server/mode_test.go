package server

import (
	"errors"
	"net"
	"path/filepath"
	"sync"
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

func TestServerModeBindConflictRollback(t *testing.T) {
	// Keep the TCP side occupied before starting the HY2 server so another
	// package's ephemeral TCP listener cannot race this fixture.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	addr := occupied.Addr().String()
	cfg := listenerTestConfig(t, config.ModeHY2)
	cfg.Listen = addr
	s, _ := startServer(t, cfg)
	require.ErrorContains(t, applyServerMode(s, config.ModeAuto), "TCP")
	require.Equal(t, config.ModeHY2, s.currentConfig().Mode)
	require.Equal(t, map[transport.Kind]string{transport.HY2: addr}, s.ListenAddresses())
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

func TestServerModeSaveFailureClosesNewListener(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := probe.Addr().String()
	require.NoError(t, probe.Close())
	cfg := listenerTestConfig(t, config.ModeHY2)
	cfg.Listen = addr
	s, err := New(cfg, nil)
	require.NoError(t, err)
	defer s.Close()
	s.cfg.ConfigPath = filepath.Join(t.TempDir(), "server.yaml")
	failure := errors.New("disk unavailable")
	s.saveConfig = func(_ string, cfg *config.ServerConfig) error {
		require.Equal(t, config.ModeAuto, cfg.Mode)
		// Binding already happened, but no greeting/Serve is allowed yet.
		c, err := net.DialTimeout("tcp", s.ListenAddr().String(), time.Second)
		require.NoError(t, err)
		defer c.Close()
		require.NoError(t, c.SetReadDeadline(time.Now().Add(20*time.Millisecond)))
		_, err = c.Read(make([]byte, 1))
		require.Error(t, err)
		return failure
	}
	require.ErrorIs(t, applyServerMode(s, config.ModeAuto), failure)
	require.Equal(t, config.ModeHY2, s.currentConfig().Mode)
	l, err := net.Listen("tcp", s.ListenAddr().String())
	require.NoError(t, err, "failed persist leaked candidate socket")
	defer l.Close()
}

func TestServerDisablingHY2LeavesTCPAndWeb(t *testing.T) {
	cfg := listenerTestConfig(t, config.ModeAuto)
	cfg.Web.Listen = "127.0.0.1:0"
	_, _, err := config.EnsureWebPassword(&cfg.Web)
	require.NoError(t, err)
	s, _ := startServer(t, cfg)
	root := tcpTestDial(t, s.ListenAddresses()[transport.TCP], "tcp", "pw", cfg.Control.Token)
	c, _ := tcpTestRegister(t, root, "tcp", cfg.Control.Token)
	tcpView, ok := s.Client("tcp")
	require.True(t, ok)
	require.Equal(t, "tcp", tcpView.Transport)
	hy := newHY2Client(t, s, "pw", nil)
	hyControl, err := hy.TCP("_hy2tun_control_:0")
	require.NoError(t, err)
	defer hyControl.Close()
	require.NoError(t, control.WriteMessage(hyControl, &control.Hello{Type: control.TypeHello, Protocol: control.ProtocolVersion, ClientID: "hy", Token: cfg.Control.Token}))
	_, err = control.ReadMessage(hyControl)
	require.NoError(t, err)
	hyView, ok := s.Client("hy")
	require.True(t, ok)
	require.Equal(t, "hy2", hyView.Transport)
	web := s.WebAddr()
	require.NoError(t, applyServerMode(s, config.ModeTCP))
	require.Equal(t, config.ModeTCP, s.Mode())
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
	require.NoError(t, applyServerMode(s, config.ModeAuto), "intentional HY2 retirement must not terminate Start")
	require.Equal(t, config.ModeAuto, s.Mode())
	newHY2Client(t, s, "pw", nil)
	requireControlPong(t, c)
}

// Close must serialize with an in-flight mode transaction: whichever wins, no
// candidate listener survives and credentials are not overwritten.
func TestServerModeCloseWinsCandidateCleanup(t *testing.T) {
	for range 8 {
		s, err := New(listenerTestConfig(t, config.ModeTCP), nil)
		require.NoError(t, err)
		generation := s.currentConfig().Credentials.Generation
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
		wg.Wait()
		require.NoError(t, closeErr)
		require.Empty(t, s.ListenAddresses())
		require.Equal(t, generation, s.currentConfig().Credentials.Generation)
		addr := s.ListenAddr().String()
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
		if modeErr != nil {
			require.ErrorContains(t, modeErr, "closed")
		}
	}
}

func TestModeUpdateRaceWithRotationAndClose(t *testing.T) {
	for range 8 {
		s, err := New(listenerTestConfig(t, config.ModeTCP), nil)
		require.NoError(t, err)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); require.NoError(t, applyServerMode(s, config.ModeAuto)) }()
		go func() {
			defer wg.Done()
			_, _, err := s.RotateCredentials("rotated-password", "rotated-control-token", "manual")
			require.NoError(t, err)
		}()
		wg.Wait()
		require.Equal(t, "rotated-password", s.currentConfig().Auth.Password)
		require.Equal(t, int64(2), s.currentConfig().Credentials.Generation)
		require.Equal(t, config.ModeAuto, s.currentConfig().Mode)
		require.NoError(t, s.Close())
		require.Error(t, applyServerMode(s, config.ModeHY2))
		require.Empty(t, s.ListenAddresses())
		udp, err := net.ListenPacket("udp", s.ListenAddr().String())
		require.NoError(t, err)
		udp.Close()
	}
}
