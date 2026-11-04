package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/transport"
)

// Keep the first RED executable even before the public mode API exists.
func applyClientMode(c any, mode config.TransportMode) error {
	u, ok := c.(interface {
		UpdateMode(config.TransportMode) error
	})
	if !ok {
		return errors.New("mode updates are not implemented")
	}
	return u.UpdateMode(mode)
}

func newTCPModeHarness(t *testing.T) *policyHarness {
	t.Helper()
	h := &policyHarness{clock: newPolicyClock(), done: make(chan error, 1), hellos: make(chan *control.Hello, 20)}
	cfg := validTestClientConfig()
	cfg.Mode = config.ModeTCP
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Policy: recoveryTestPolicy(), Clock: h.clock, Jitter: func(d time.Duration) time.Duration { return d }, Dial: func(ctx context.Context, kind transport.Kind, cfg *config.ClientConfig) (transport.Session, error) {
		if kind == transport.HY2 {
			return h.dial(ctx, kind, cfg)
		}
		h.mu.Lock()
		h.kinds = append(h.kinds, kind)
		h.mu.Unlock()
		s := newSessionStub(kind)
		s.control = h.control(kind)
		return s, nil
	}})
	require.NoError(t, err)
	h.client = cl
	t.Cleanup(func() { require.NoError(t, cl.Close()); <-h.done; synctest.Wait() })
	go func() { h.done <- cl.Start(context.Background()) }()
	synctest.Wait()
	return h
}

func TestClientModeSaveFailureKeepsTransport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTCPModeHarness(t)
		old := h.client.currentSession()
		before := h.client.TransportState()
		failure := errors.New("disk unavailable")
		h.client.SetPersist(func(*config.ClientConfig) error { return failure })
		require.ErrorIs(t, applyClientMode(h.client, config.ModeHY2), failure)
		synctest.Wait()
		require.Same(t, old, h.client.currentSession())
		require.Equal(t, before, h.client.TransportState())
		require.True(t, h.client.Status().Connected)
	})
}

func TestModeAutoKeepsAllowedTCPAndStartsProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTCPModeHarness(t)
		old := h.client.currentSession()
		require.NoError(t, applyClientMode(h.client, config.ModeAuto))
		synctest.Wait()
		require.Same(t, old, h.client.currentSession())
		require.Equal(t, "tcp", h.client.Status().Transport.ActiveTransport)
		h.tick(1)
		require.Equal(t, int32(1), h.probeCalls.Load())
		require.Same(t, old, h.client.currentSession())
		require.NoError(t, applyClientMode(h.client, config.ModeTCP))
		synctest.Wait()
		h.tick(4)
		require.Equal(t, int32(1), h.probeCalls.Load(), "forced TCP must stop probe policy, not its owner")
		require.Same(t, old, h.client.currentSession())
		require.NoError(t, applyClientMode(h.client, config.ModeAuto))
		synctest.Wait()
		h.tick(1)
		require.Equal(t, int32(2), h.probeCalls.Load(), "same controller must restart probing")
	})
}

func TestModeChangeDuringCandidateHandoffResumesProbing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := recoveryTestPolicy()
		p.MinTCPHold = time.Second
		p.RecoverySuccesses = 1
		h := newPolicyHarness(t, p)
		h.client.SetPersist(func(*config.ClientConfig) error { return nil })
		// Fail later probes so the resumed loop stays on the retained owner
		// instead of immediately handing off to another HY2 session.
		h.gate = func(context.Context) error {
			if h.probeCalls.Load() > 2 {
				return syscall.ECONNREFUSED
			}
			return nil
		}
		old := h.client.currentSession()
		var hookRan, hookDone atomic.Bool
		var hookErr atomic.Value
		h.client.handoffHook.Store(func() {
			if !hookRan.CompareAndSwap(false, true) {
				return
			}
			if err := applyClientMode(h.client, config.ModeTCP); err != nil {
				hookErr.Store(err)
			}
			if err := applyClientMode(h.client, config.ModeAuto); err != nil {
				hookErr.Store(err)
			}
			hookDone.Store(true)
		})

		h.tick(1)
		require.True(t, hookRan.Load(), "handoff hook did not run")
		require.True(t, hookDone.Load(), "handoff hook did not finish")
		if err, _ := hookErr.Load().(error); err != nil {
			require.NoError(t, err)
		}
		require.Equal(t, int32(2), h.probeCalls.Load(), "first probe plus handoff candidate")
		require.Same(t, old, h.client.currentSession(), "mode toggle must retain the TCP owner")

		h.tick(1)
		require.Greater(t, h.probeCalls.Load(), int32(2), "auto must resume probing on the retained owner")
		require.Same(t, old, h.client.currentSession())
	})
}

func TestClientModeDisallowedOwnerReconnects(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTCPModeHarness(t)
		old := h.client.currentSession().(*sessionStub)
		require.NoError(t, applyClientMode(h.client, config.ModeHY2))
		synctest.Wait()
		select {
		case <-old.closed:
		default:
			t.Fatal("disallowed TCP root remained live")
		}
		require.Equal(t, "hy2", h.client.Status().Transport.ActiveTransport)
		require.Equal(t, config.ModeHY2, h.client.TransportState().Mode)
	})
}

func TestSupervisorModeOnlyChangesSelectedWorker(t *testing.T) {
	cfg := multiClientConfigForTest()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()
	other := sup.workers["us"].snapshotConfig()
	selected := sup.workers["sg"].snapshotConfig()
	web := sup.web
	api, ok := sup.Server("sg")
	require.True(t, ok)
	require.NoError(t, applyClientMode(api, config.ModeTCP))
	require.Equal(t, other, sup.workers["us"].snapshotConfig())
	require.Same(t, web, sup.web)
	selected.Mode = config.ModeTCP
	require.Equal(t, selected, sup.workers["sg"].snapshotConfig())
	require.NoError(t, sup.workers["sg"].UpdateName("renamed"))
	data, err := os.ReadFile(cfg.ConfigPath)
	require.NoError(t, err)
	var saved config.ClientConfig
	require.NoError(t, yaml.Unmarshal(data, &saved))
	require.Equal(t, config.ModeTCP, saved.Servers[1].Mode)
	require.Equal(t, "renamed", saved.Servers[1].Client.Name)
	require.Equal(t, cfg.Servers[0], saved.Servers[0])
}

func TestSupervisorModeDefaultPersistsLegacyShape(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()
	require.NoError(t, applyClientMode(sup, config.ModeTCP))
	require.NoError(t, sup.UpdateName("renamed"))
	data, err := os.ReadFile(cfg.ConfigPath)
	require.NoError(t, err)
	var saved config.ClientConfig
	require.NoError(t, yaml.Unmarshal(data, &saved))
	require.Equal(t, config.ModeTCP, saved.Mode)
	require.Empty(t, saved.Servers)
}

func TestModeUpdateRaceWithRotationAndClose(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer cl.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	var modeErr, rotateErr error
	go func() {
		defer wg.Done()
		modeErr = applyClientMode(cl, config.ModeTCP)
	}()
	go func() {
		defer wg.Done()
		rotateErr = cl.RotateLocalCredentials(2, "rotated-password", "rotated-control-token")
	}()
	wg.Wait()
	require.NoError(t, modeErr)
	require.NoError(t, rotateErr)
	require.Equal(t, "rotated-password", cl.CredentialsSnapshot().Auth)
	require.Equal(t, config.ModeTCP, cl.TransportState().Mode)
	require.NoError(t, cl.Close())
	require.Error(t, applyClientMode(cl, config.ModeAuto))
	require.Equal(t, config.ModeTCP, cl.TransportState().Mode)
}

// Split lock acquisitions can combine connected=true with an offline transport
// or pair one config's metadata with another config's credentials.
func TestClientModeStatusTransitionSnapshots(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 30000; i++ {
			cl.configMu.Lock()
			cl.mu.Lock()
			if i%2 == 0 {
				cl.cfg.Client.Name = "online"
				cl.connected = true
				cl.transportState.ActiveTransport = "tcp"
				cl.credentialsGeneration = 2
			} else {
				cl.cfg.Client.Name = "offline"
				cl.connected = false
				cl.transportState.ActiveTransport = ""
				cl.credentialsGeneration = 3
			}
			cl.mu.Unlock()
			cl.configMu.Unlock()
		}
	}()
	defer func() { <-done }()
	for {
		s := cl.Status()
		if s.Name == "online" {
			require.True(t, s.Connected)
			require.Equal(t, "tcp", s.Transport.ActiveTransport)
			require.Equal(t, int64(2), s.CredentialGeneration)
		} else if s.Name == "offline" {
			require.False(t, s.Connected)
			require.Empty(t, s.Transport.ActiveTransport)
			require.Equal(t, int64(3), s.CredentialGeneration)
		}
		select {
		case <-done:
			return
		default:
		}
	}
}
