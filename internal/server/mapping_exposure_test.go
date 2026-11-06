package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/ports"
	"hy2tun/internal/state"
	"hy2tun/internal/view"
)

// exposureEnv is a mapping manager backed by a real temporary state file.
type exposureEnv struct {
	manager  *mappingManager
	store    *state.Store
	path     string
	bindHost string
}

func newExposureEnv(t *testing.T, bindHost string) *exposureEnv {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(path, time.Hour)
	require.NoError(t, err)
	policy, err := ports.NewPolicy(nil, nil, "")
	require.NoError(t, err)
	manager := newMappingManager(policy, store, slog.Default(), bindHost, nil, newDataStreams())
	t.Cleanup(func() {
		manager.closeAll()
		_ = store.Close()
	})
	return &exposureEnv{manager: manager, store: store, path: path, bindHost: bindHost}
}

func newExposureSession(id string) *clientSession {
	return &clientSession{
		id:            id,
		online:        true,
		credentialAck: make(chan *control.CredentialAck, 1),
		ready:         make(chan struct{}),
		done:          make(chan struct{}),
	}
}

type exposureRuntimeState struct {
	runtime      *mappingRuntime
	exposure     view.Exposure
	status       string
	errCode      string
	assignedPort int
	listeners    *mappingListenerSet
}

func snapshotExposureRuntime(t *testing.T, m *mappingManager, clientID, mappingID string) exposureRuntimeState {
	t.Helper()

	m.mu.Lock()
	defer m.mu.Unlock()
	sess := m.sessions[clientID]
	require.NotNil(t, sess, "client %q is not live", clientID)
	rt := sess.mappings[mappingID]
	require.NotNil(t, rt, "mapping %q is not live", mappingID)
	return exposureRuntimeState{
		runtime:      rt,
		exposure:     rt.exposure,
		status:       rt.status,
		errCode:      rt.errCode,
		assignedPort: rt.assignedPort,
		listeners:    rt.listeners,
	}
}

func registerExposureMapping(t *testing.T, m *mappingManager, sess *clientSession, mapping config.Mapping) exposureRuntimeState {
	t.Helper()

	results := m.register(sess, []config.Mapping{mapping})
	require.Len(t, results, 1)
	require.True(t, results[0].OK, "register mapping %s: %+v", mapping.ID, results[0])
	return snapshotExposureRuntime(t, m, sess.id, mapping.ID)
}

func updateExposureMapping(t *testing.T, m *mappingManager, sess *clientSession, revision int64, mappings ...config.Mapping) {
	t.Helper()

	results, accepted, resync := m.update(sess, revision, mappings)
	require.True(t, accepted)
	require.False(t, resync)
	for _, result := range results {
		require.True(t, result.OK, "update mapping %s: %+v", result.ID, result)
	}
}

// freeExposureTCPPort returns a port that was free on the dual-stack wildcard.
func freeExposureTCPPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}

func freeExposureUDPPort(t *testing.T) int {
	t.Helper()

	conn, err := net.ListenPacket("udp", ":0")
	require.NoError(t, err)
	port := conn.LocalAddr().(*net.UDPAddr).Port
	require.NoError(t, conn.Close())
	return port
}

func requireLoopbackListeners(t *testing.T, set *mappingListenerSet) {
	t.Helper()

	require.NotNil(t, set)
	require.NotEmpty(t, set.TCP)
	for _, listener := range set.TCP {
		host, _, err := net.SplitHostPort(listener.Addr().String())
		require.NoError(t, err)
		ip := net.ParseIP(host)
		require.NotNil(t, ip)
		require.True(t, ip.IsLoopback(), "listener %s must be loopback", listener.Addr())
	}
}

func requirePublicListeners(t *testing.T, set *mappingListenerSet) {
	t.Helper()

	require.NotNil(t, set)
	require.NotEmpty(t, set.TCP)
	for _, listener := range set.TCP {
		host, _, err := net.SplitHostPort(listener.Addr().String())
		require.NoError(t, err)
		ip := net.ParseIP(host)
		require.NotNil(t, ip)
		require.False(t, ip.IsLoopback(), "listener %s must not be loopback", listener.Addr())
		require.True(t, ip.IsUnspecified(), "public wildcard listener expected, got %s", listener.Addr())
	}
}

func requireSpecificListeners(t *testing.T, set *mappingListenerSet, wantHost string) {
	t.Helper()

	require.NotNil(t, set)
	require.NotEmpty(t, set.TCP)
	for _, listener := range set.TCP {
		host, _, err := net.SplitHostPort(listener.Addr().String())
		require.NoError(t, err)
		require.Equal(t, wantHost, host)
	}
}

func requireLoopbackUDP(t *testing.T, set *mappingListenerSet) {
	t.Helper()

	require.NotNil(t, set)
	require.NotNil(t, set.UDP)
	require.NotEmpty(t, set.UDP.conns)
	for _, conn := range set.UDP.conns {
		host, _, err := net.SplitHostPort(conn.conn.LocalAddr().String())
		require.NoError(t, err)
		ip := net.ParseIP(host)
		require.NotNil(t, ip)
		require.True(t, ip.IsLoopback(), "UDP socket %s must be loopback", conn.conn.LocalAddr())
	}
}

func requirePublicUDP(t *testing.T, set *mappingListenerSet) {
	t.Helper()

	require.NotNil(t, set)
	require.NotNil(t, set.UDP)
	require.NotEmpty(t, set.UDP.conns)
	for _, conn := range set.UDP.conns {
		host, _, err := net.SplitHostPort(conn.conn.LocalAddr().String())
		require.NoError(t, err)
		ip := net.ParseIP(host)
		require.NotNil(t, ip)
		require.True(t, ip.IsUnspecified(), "public UDP wildcard expected, got %s", conn.conn.LocalAddr())
	}
}

func trackExposureConn(t *testing.T, rt *mappingRuntime) *trackingConn {
	t.Helper()

	active, remote := newTrackingConn()
	t.Cleanup(func() {
		_ = active.Close()
		_ = remote.Close()
	})
	rt.trackActive(active)
	return active
}

func mappingViewByID(t *testing.T, client view.ClientView, mappingID string) view.MappingView {
	t.Helper()

	for _, mapping := range client.Mappings {
		if mapping.ID == mappingID {
			return mapping
		}
	}
	t.Fatalf("missing mapping view %q", mappingID)
	return view.MappingView{}
}

func TestMappingExposureDefaultPublic(t *testing.T) {
	env := newExposureEnv(t, "")
	sess := newExposureSession("client")
	port := freeExposureTCPPort(t)
	mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}

	before := registerExposureMapping(t, env.manager, sess, mapping)
	require.Equal(t, view.ExposurePublic, before.exposure)
	require.Equal(t, "running", before.status)
	requirePublicListeners(t, before.listeners)
	require.Equal(t, "", env.store.Exposure("client", "m1"), "the public default needs no stored entry")
	require.Equal(t, view.ExposurePublic, env.manager.exposureFor("client", "m1"))

	clients := env.manager.clientViews()
	require.Len(t, clients, 1)
	require.Equal(t, view.ExposurePublic, mappingViewByID(t, clients[0], "m1").Exposure)

	_, err := env.manager.SetExposure("client", "m1", view.Exposure(""))
	require.ErrorIs(t, err, ErrExposureInvalid)
	_, err = env.manager.SetExposure("client", "m1", view.Exposure("bogus"))
	require.ErrorIs(t, err, ErrExposureInvalid)
	_, err = env.manager.SetExposure("client", "missing", view.ExposureLocal)
	require.ErrorIs(t, err, ErrMappingNotFound)
	_, err = env.manager.SetExposure("missing", "m1", view.ExposureLocal)
	require.ErrorIs(t, err, control.ErrClientNotFound)

	// Invalid input must not have closed or rebound the running mapping.
	after := snapshotExposureRuntime(t, env.manager, "client", "m1")
	require.Same(t, before.runtime, after.runtime)
	require.Same(t, before.listeners, after.listeners)
	require.Equal(t, view.ExposurePublic, after.exposure)
}

func TestMappingExposureSwitchLocal(t *testing.T) {
	t.Run("tcp", func(t *testing.T) {
		env := newExposureEnv(t, "")
		sess := newExposureSession("client")
		port := freeExposureTCPPort(t)
		mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}

		before := registerExposureMapping(t, env.manager, sess, mapping)
		requirePublicListeners(t, before.listeners)
		beforeGeneration := before.runtime.acceptGen.Load()
		active := trackExposureConn(t, before.runtime)

		switched, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.NoError(t, err)
		require.Equal(t, view.ExposureLocal, switched.Exposure)
		require.Equal(t, "running", switched.Status)
		require.True(t, active.closed.Load(), "an existing public connection must be closed")
		require.Equal(t, "local", env.store.Exposure("client", "m1"))
		require.Greater(t, before.runtime.acceptGen.Load(), beforeGeneration, "a switch must retire the old accept generation")

		local := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Equal(t, view.ExposureLocal, local.exposure)
		require.Equal(t, "running", local.status)
		requireLoopbackListeners(t, local.listeners)
		require.NotSame(t, before.listeners, local.listeners)
		require.Error(t, netDialTimeout(fmt.Sprintf("127.0.0.2:%d", port)), "no public listener may remain after switching to local")

		activeLocal := trackExposureConn(t, local.runtime)
		switched, err = env.manager.SetExposure("client", "m1", view.ExposurePublic)
		require.NoError(t, err)
		require.Equal(t, view.ExposurePublic, switched.Exposure)
		require.True(t, activeLocal.closed.Load(), "an existing loopback connection must be closed")
		require.Equal(t, "public", env.store.Exposure("client", "m1"))

		public := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Equal(t, view.ExposurePublic, public.exposure)
		requirePublicListeners(t, public.listeners)
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	})

	t.Run("udp", func(t *testing.T) {
		env := newExposureEnv(t, "")
		sess := newExposureSession("client")
		port := freeExposureUDPPort(t)
		mapping := config.Mapping{ID: "m1", Type: "udp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}

		before := registerExposureMapping(t, env.manager, sess, mapping)
		requirePublicUDP(t, before.listeners)

		_, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.NoError(t, err)
		require.Equal(t, "local", env.store.Exposure("client", "m1"))
		local := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Equal(t, view.ExposureLocal, local.exposure)
		requireLoopbackUDP(t, local.listeners)

		// The old public fan-in must be closed, not merely replaced.
		_, _, readErr := before.listeners.UDP.ReadFrom(make([]byte, 1))
		require.Error(t, readErr)
		require.ErrorIs(t, readErr, net.ErrClosed)

		_, err = env.manager.SetExposure("client", "m1", view.ExposurePublic)
		require.NoError(t, err)
		require.Equal(t, "public", env.store.Exposure("client", "m1"))
		public := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Equal(t, view.ExposurePublic, public.exposure)
		requirePublicUDP(t, public.listeners)
	})
}

func TestMappingExposurePartialFamily(t *testing.T) {
	// A specific public bind host leaves the IPv6 loopback address free while
	// the mapping is public, so the test can occupy exactly one local family.
	env := newExposureEnv(t, "127.0.0.2")
	sess := newExposureSession("client")
	port := freeExposureTCPPort(t)
	mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}
	before := registerExposureMapping(t, env.manager, sess, mapping)
	requireSpecificListeners(t, before.listeners, "127.0.0.2")

	// Occupy the IPv6 loopback address so only the IPv4 loopback family can
	// bind. The switch must succeed with a stable warning, not silently claim
	// both families are active.
	ipv6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	defer ipv6.Close()

	switched, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
	require.NoError(t, err)
	require.Equal(t, view.ExposureLocal, switched.Exposure)
	require.Equal(t, "running", switched.Status)
	require.Equal(t, []string{"ipv6"}, switched.UnavailableFamilies)
	require.Equal(t, "local", env.store.Exposure("client", "m1"))

	partial := snapshotExposureRuntime(t, env.manager, "client", "m1")
	require.Equal(t, view.ExposureLocal, partial.exposure)
	require.Equal(t, []string{"ipv6"}, partial.listeners.Warnings())
	requireLoopbackListeners(t, partial.listeners)
	for _, listener := range partial.listeners.TCP {
		host, _, splitErr := net.SplitHostPort(listener.Addr().String())
		require.NoError(t, splitErr)
		require.Equal(t, "127.0.0.1", host, "the available family must stay bound")
	}
	clients := env.manager.clientViews()
	require.Equal(t, []string{"ipv6"}, mappingViewByID(t, clients[0], "m1").UnavailableFamilies)

	// Releasing the family and rebinding public recomputes the warning.
	require.NoError(t, ipv6.Close())
	_, err = env.manager.SetExposure("client", "m1", view.ExposurePublic)
	require.NoError(t, err)
	recovered := snapshotExposureRuntime(t, env.manager, "client", "m1")
	require.Empty(t, recovered.listeners.Warnings(), "the warning must clear when the family binds again")
	requireSpecificListeners(t, recovered.listeners, "127.0.0.2")
}

func netDialTimeout(addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
	}
	return err
}

func TestMappingExposureRollback(t *testing.T) {
	t.Run("public to local bind failure restores public", func(t *testing.T) {
		env := newExposureEnv(t, "127.0.0.2")
		sess := newExposureSession("client")
		port := freeExposureTCPPort(t)
		mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}

		before := registerExposureMapping(t, env.manager, sess, mapping)
		requireSpecificListeners(t, before.listeners, "127.0.0.2")
		active := trackExposureConn(t, before.runtime)

		// Occupy both loopback families so the local bind fails completely.
		ipv4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		require.NoError(t, err)
		defer ipv4.Close()
		ipv6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
		if err != nil {
			t.Skipf("IPv6 loopback is unavailable: %v", err)
		}
		defer ipv6.Close()

		switched, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.Error(t, err)
		var switchErr *ExposureSwitchError
		require.ErrorAs(t, err, &switchErr)
		require.Equal(t, view.ExposureLocal, switchErr.Requested)
		require.Equal(t, view.ExposurePublic, switchErr.Exposure)
		require.False(t, switchErr.Closed)
		require.False(t, switchErr.Persistence)
		require.True(t, switchErr.ExposureConflict())
		require.Equal(t, view.ExposurePublic, switched.Exposure)
		require.Equal(t, "running", switched.Status)
		require.True(t, active.closed.Load())

		restored := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Equal(t, view.ExposurePublic, restored.exposure)
		require.Equal(t, "running", restored.status)
		requireSpecificListeners(t, restored.listeners, "127.0.0.2")
		require.Equal(t, "", env.store.Exposure("client", "m1"), "the failed switch must not persist local")

		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.2:%d", port), time.Second)
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	})

	t.Run("local to public bind failure restores local", func(t *testing.T) {
		env := newExposureEnv(t, "127.0.0.2")
		sess := newExposureSession("client")
		port := freeExposureTCPPort(t)
		mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}

		registerExposureMapping(t, env.manager, sess, mapping)
		_, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.NoError(t, err)
		local := snapshotExposureRuntime(t, env.manager, "client", "m1")
		requireLoopbackListeners(t, local.listeners)
		active := trackExposureConn(t, local.runtime)

		// The public specific address is occupied, so public cannot rebind.
		occupied, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.2:%d", port))
		require.NoError(t, err)
		defer occupied.Close()

		switched, err := env.manager.SetExposure("client", "m1", view.ExposurePublic)
		require.Error(t, err)
		var switchErr *ExposureSwitchError
		require.ErrorAs(t, err, &switchErr)
		require.Equal(t, view.ExposurePublic, switchErr.Requested)
		require.Equal(t, view.ExposureLocal, switchErr.Exposure)
		require.False(t, switchErr.Closed)
		require.False(t, switchErr.Persistence)
		require.True(t, switchErr.ExposureConflict())
		require.Equal(t, view.ExposureLocal, switched.Exposure)
		require.True(t, active.closed.Load())

		restored := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Equal(t, view.ExposureLocal, restored.exposure)
		require.Equal(t, "running", restored.status)
		requireLoopbackListeners(t, restored.listeners)
		require.Equal(t, "local", env.store.Exposure("client", "m1"))

		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	})

	t.Run("persistence failure restores previous listeners", func(t *testing.T) {
		env := newExposureEnv(t, "")
		sess := newExposureSession("client")
		port := freeExposureTCPPort(t)
		mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}

		before := registerExposureMapping(t, env.manager, sess, mapping)
		requirePublicListeners(t, before.listeners)
		active := trackExposureConn(t, before.runtime)

		// Removing the state directory makes the exposure save fail before
		// the rename, so the transaction must roll back to public.
		require.NoError(t, os.RemoveAll(filepath.Dir(env.path)))
		switched, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.Error(t, err)
		var switchErr *ExposureSwitchError
		require.ErrorAs(t, err, &switchErr)
		require.Equal(t, view.ExposureLocal, switchErr.Requested)
		require.Equal(t, view.ExposurePublic, switchErr.Exposure)
		require.False(t, switchErr.Closed)
		require.True(t, switchErr.Persistence)
		require.False(t, switchErr.ExposureConflict())
		require.Equal(t, view.ExposurePublic, switched.Exposure)
		require.True(t, active.closed.Load())

		restored := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Equal(t, view.ExposurePublic, restored.exposure)
		require.Equal(t, "running", restored.status)
		requirePublicListeners(t, restored.listeners)
		require.Equal(t, "", env.store.Exposure("client", "m1"))

		// Once persistence works again the same switch succeeds.
		require.NoError(t, os.MkdirAll(filepath.Dir(env.path), 0700))
		_, err = env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.NoError(t, err)
		final := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Equal(t, view.ExposureLocal, final.exposure)
		requireLoopbackListeners(t, final.listeners)
		require.Equal(t, "local", env.store.Exposure("client", "m1"))
	})
}

func TestMappingExposureSameValue(t *testing.T) {
	t.Run("healthy mapping does not rebuild", func(t *testing.T) {
		env := newExposureEnv(t, "")
		sess := newExposureSession("client")
		port := freeExposureTCPPort(t)
		mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}

		before := registerExposureMapping(t, env.manager, sess, mapping)
		active := trackExposureConn(t, before.runtime)

		switched, err := env.manager.SetExposure("client", "m1", view.ExposurePublic)
		require.NoError(t, err)
		require.Equal(t, view.ExposurePublic, switched.Exposure)
		require.False(t, active.closed.Load(), "a same-value public request must not close connections")
		afterPublic := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Same(t, before.runtime, afterPublic.runtime)
		require.Same(t, before.listeners, afterPublic.listeners)

		_, err = env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.NoError(t, err)
		local := snapshotExposureRuntime(t, env.manager, "client", "m1")
		activeLocal := trackExposureConn(t, local.runtime)

		_, err = env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.NoError(t, err)
		require.False(t, activeLocal.closed.Load(), "a same-value local request must not close connections")
		afterLocal := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Same(t, local.runtime, afterLocal.runtime)
		require.Same(t, local.listeners, afterLocal.listeners)
	})

	t.Run("warning retry still rebuilds", func(t *testing.T) {
		env := newExposureEnv(t, "127.0.0.2")
		sess := newExposureSession("client")
		port := freeExposureTCPPort(t)
		mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}
		registerExposureMapping(t, env.manager, sess, mapping)

		ipv6, err := net.Listen("tcp6", fmt.Sprintf("[::1]:%d", port))
		if err != nil {
			t.Skipf("IPv6 loopback is unavailable: %v", err)
		}
		defer ipv6.Close()

		switched, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.NoError(t, err)
		require.Equal(t, []string{"ipv6"}, switched.UnavailableFamilies)
		partial := snapshotExposureRuntime(t, env.manager, "client", "m1")

		require.NoError(t, ipv6.Close())
		retried, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.NoError(t, err)
		require.Empty(t, retried.UnavailableFamilies, "a same-value retry must clear a recovered warning")
		recovered := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.NotSame(t, partial.listeners, recovered.listeners)
		require.Empty(t, recovered.listeners.Warnings())
		requireLoopbackListeners(t, recovered.listeners)
	})
}

func TestMappingExposureStaleAcceptGeneration(t *testing.T) {
	srv := &Server{log: slog.Default(), data: newDataStreams()}
	sess := newExposureSession("client")
	rt := &mappingRuntime{
		id:      "m1",
		mapping: config.Mapping{ID: "m1", Type: "tcp", Enabled: true},
		status:  mappingStatusRunning,
		data:    srv.data,
		connSem: make(chan struct{}, 1),
	}
	oldGeneration := rt.acceptGen.Load()

	// 1. The accept loop must drop a connection accepted before retirement.
	// acceptGen.Add(1) models switchExposureLocked's retirement; the real
	// switch increment is asserted in TestMappingExposureSwitchLocal/tcp.
	client, server := net.Pipe()
	listener := newExposureGenListener(server)
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan struct{})
	go func() {
		srv.serveTCP(nil, rt, listener, oldGeneration)
		close(done)
	}()
	<-listener.accepted
	rt.acceptGen.Add(1)
	close(listener.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveTCP did not return after a stale generation")
	}
	requireExposureConnClosed(t, client)
	require.NoError(t, client.Close())
	require.NoError(t, listener.Close())

	// 2. An already-accepted connection must be rejected before tracking.
	client2, server2 := net.Pipe()
	srv.openPublicConn(sess, rt, server2, oldGeneration)
	requireExposureConnClosed(t, client2)
	require.NoError(t, client2.Close())
	require.Zero(t, exposureActiveCount(rt))
	require.Len(t, rt.connSem, 0)
	require.Zero(t, exposurePendingCount(srv.data))

	// 3. A tracked connection must be rejected immediately before opening the
	// reverse data stream.
	client3, server3 := net.Pipe()
	active := &activeConn{Conn: server3, mapping: rt}
	rt.trackActive(active)
	err := srv.openDataStream(sess, rt, active, oldGeneration)
	require.Error(t, err)
	require.Zero(t, exposurePendingCount(srv.data))
	require.NoError(t, active.Close())
	require.NoError(t, client3.Close())

	// 4. A pending stream added after retirement must be rejected at take time.
	client4, server4 := net.Pipe()
	token, err := srv.data.add(&pendingConn{
		conn:       server4,
		mapping:    rt,
		owner:      sess,
		expires:    time.Now().Add(time.Second),
		generation: oldGeneration,
	})
	require.NoError(t, err)
	_, err = srv.data.take(token)
	require.Error(t, err)
	requireExposureConnClosed(t, client4)
	require.NoError(t, client4.Close())
}

type exposureGenListener struct {
	conn     net.Conn
	accepted chan struct{}
	release  chan struct{}
	closed   chan struct{}
	once     sync.Once
}

func newExposureGenListener(conn net.Conn) *exposureGenListener {
	return &exposureGenListener{
		conn:     conn,
		accepted: make(chan struct{}),
		release:  make(chan struct{}),
		closed:   make(chan struct{}),
	}
}

func (l *exposureGenListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.accepted) })
	select {
	case <-l.release:
		return l.conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *exposureGenListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *exposureGenListener) Addr() net.Addr { return &net.TCPAddr{} }

func requireExposureConnClosed(t *testing.T, conn net.Conn) {
	t.Helper()

	// net.Pipe returns io.ErrClosedPipe from SetReadDeadline once the peer is
	// gone; that is itself evidence of closure.
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, err := conn.Read(make([]byte, 1))
	require.Error(t, err)
	var netErr net.Error
	require.False(t, errors.As(err, &netErr) && netErr.Timeout(), "connection must be closed, not left open")
}

func exposureActiveCount(rt *mappingRuntime) int {
	rt.activeMu.Lock()
	defer rt.activeMu.Unlock()
	return len(rt.active)
}

func exposurePendingCount(data *dataStreams) int {
	data.mu.Lock()
	defer data.mu.Unlock()
	return len(data.pending)
}

func TestMappingExposurePersistReleasesManagerLock(t *testing.T) {
	env := newExposureEnv(t, "")
	sess := newExposureSession("client")
	port := freeExposureTCPPort(t)
	mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}
	registerExposureMapping(t, env.manager, sess, mapping)

	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	env.manager.persistExposureFn = func(clientID, mappingID string, exposure view.Exposure) error {
		close(entered)
		<-release
		return env.store.SetExposure(clientID, mappingID, string(exposure))
	}
	t.Cleanup(func() { env.manager.persistExposureFn = nil })

	done := make(chan error, 1)
	go func() {
		_, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("exposure persistence was not called")
	}

	// The manager lock must be free while the state fsync is in progress.
	acquired := make(chan struct{})
	go func() {
		env.manager.mu.Lock()
		env.manager.mu.Unlock()
		close(acquired)
	}()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("m.mu was held across exposure persistence")
	}

	close(release)
	released = true
	require.NoError(t, <-done)
	final := snapshotExposureRuntime(t, env.manager, "client", "m1")
	require.Equal(t, view.ExposureLocal, final.exposure)
	requireLoopbackListeners(t, final.listeners)
	require.Equal(t, "local", env.store.Exposure("client", "m1"))
}

func TestMappingExposurePersistRecheckFailure(t *testing.T) {
	env := newExposureEnv(t, "")
	sess := newExposureSession("client")
	port := freeExposureTCPPort(t)
	mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}
	registerExposureMapping(t, env.manager, sess, mapping)

	entered := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	env.manager.persistExposureFn = func(clientID, mappingID string, exposure view.Exposure) error {
		close(entered)
		<-release
		return env.store.SetExposure(clientID, mappingID, string(exposure))
	}
	t.Cleanup(func() { env.manager.persistExposureFn = nil })

	done := make(chan error, 1)
	go func() {
		_, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("exposure persistence was not called")
	}

	// Replace/remove the session while the manager lock is released. The
	// transaction must not serve the set it bound.
	env.manager.detach(sess)
	close(release)
	released = true

	err := <-done
	require.Error(t, err)
	var switchErr *ExposureSwitchError
	require.ErrorAs(t, err, &switchErr)
	require.True(t, switchErr.Closed)
	require.False(t, switchErr.Persistence)
	require.True(t, switchErr.ExposureConflict())
	require.ErrorIs(t, switchErr, control.ErrClientOffline)
	require.Equal(t, "local", env.store.Exposure("client", "m1"), "the requested value remains the offline desired state")

	// The bound-but-unpublished set was closed; the port is free again.
	listener, listenErr := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, listenErr)
	require.NoError(t, listener.Close())
}

func TestMappingExposureDoesNotBlockRegistrationWhileControlMuHeld(t *testing.T) {
	env := newExposureEnv(t, "")
	sessA := newExposureSession("a")
	portA := freeExposureTCPPort(t)
	mappingA := config.Mapping{ID: "m1", Type: "tcp", RemotePort: portA, LocalAddr: "127.0.0.1:9001", Enabled: true}
	registerExposureMapping(t, env.manager, sessA, mappingA)

	sessB := newExposureSession("b")
	portB := freeExposureTCPPort(t)
	mappingB := config.Mapping{ID: "m1", Type: "tcp", RemotePort: portB, LocalAddr: "127.0.0.1:9002", Enabled: true}

	entered := make(chan struct{})
	var once sync.Once
	env.manager.setExposureBeforeControlMu = func() { once.Do(func() { close(entered) }) }
	t.Cleanup(func() { env.manager.setExposureBeforeControlMu = nil })

	// Simulate a slow push/update holding client A's control queue.
	sessA.controlMu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			sessA.controlMu.Unlock()
		}
	}()

	exposureDone := make(chan error, 1)
	go func() {
		_, err := env.manager.SetExposure("a", "m1", view.ExposureLocal)
		exposureDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("SetExposure did not reach the controlMu wait")
	}

	// SetExposure is now blocked on A's controlMu. An unrelated registration
	// must complete because exposureMu is not held while waiting for controlMu.
	registrationDone := make(chan struct{})
	go func() {
		env.manager.register(sessB, []config.Mapping{mappingB})
		close(registrationDone)
	}()
	select {
	case <-registrationDone:
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated registration blocked behind SetExposure waiting on controlMu")
	}
	env.manager.mu.Lock()
	_, registered := env.manager.sessions["b"]
	env.manager.mu.Unlock()
	require.True(t, registered)

	// Release the simulated push; the switch can now take controlMu and finish.
	sessA.controlMu.Unlock()
	unlocked = true
	require.NoError(t, <-exposureDone)
	final := snapshotExposureRuntime(t, env.manager, "a", "m1")
	require.Equal(t, view.ExposureLocal, final.exposure)
	requireLoopbackListeners(t, final.listeners)
}

func TestMappingExposurePersistsAcrossReconnect(t *testing.T) {
	env := newExposureEnv(t, "")
	port := freeExposureTCPPort(t)
	mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}

	sess := newExposureSession("client")
	registerExposureMapping(t, env.manager, sess, mapping)

	// Take the client offline first: the stored value is then the desired
	// exposure for the next bind, with no live listener to switch.
	env.manager.detach(sess)
	offlineView, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
	require.NoError(t, err)
	require.Equal(t, view.ExposureLocal, offlineView.Exposure)
	require.Equal(t, mappingStatusOffline, offlineView.Status)
	require.Equal(t, "local", env.store.Exposure("client", "m1"))

	offlineClients := env.manager.clientViews()
	require.Len(t, offlineClients, 1)
	require.False(t, offlineClients[0].Online)
	require.Equal(t, view.ExposureLocal, mappingViewByID(t, offlineClients[0], "m1").Exposure)
	require.Equal(t, mappingStatusOffline, mappingViewByID(t, offlineClients[0], "m1").Status)

	reconnected := newExposureSession("client")
	afterReconnect := registerExposureMapping(t, env.manager, reconnected, mapping)
	require.Equal(t, view.ExposureLocal, afterReconnect.exposure)
	require.Equal(t, "running", afterReconnect.status)
	requireLoopbackListeners(t, afterReconnect.listeners)

	// Simulate a server restart with the same state file.
	env.manager.closeAll()
	require.NoError(t, env.store.Close())
	reopened, err := state.Open(env.path, time.Hour)
	require.NoError(t, err)
	policy, err := ports.NewPolicy(nil, nil, "")
	require.NoError(t, err)
	restarted := newMappingManager(policy, reopened, slog.Default(), "", nil, newDataStreams())
	t.Cleanup(func() {
		restarted.closeAll()
		_ = reopened.Close()
	})

	restartedSession := newExposureSession("client")
	afterRestart := registerExposureMapping(t, restarted, restartedSession, mapping)
	require.Equal(t, view.ExposureLocal, afterRestart.exposure)
	require.Equal(t, "running", afterRestart.status)
	requireLoopbackListeners(t, afterRestart.listeners)
}

func TestMappingExposureAllBindPaths(t *testing.T) {
	t.Run("client update enable and retry", func(t *testing.T) {
		env := newExposureEnv(t, "")
		sess := newExposureSession("client")
		port := freeExposureTCPPort(t)
		mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}

		before := registerExposureMapping(t, env.manager, sess, mapping)
		_, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.NoError(t, err)
		afterSwitch := snapshotExposureRuntime(t, env.manager, "client", "m1")

		// A note-only client update keeps the same runtime and local exposure.
		noteOnly := mapping
		noteOnly.Note = "changed"
		updateExposureMapping(t, env.manager, sess, 1, noteOnly)
		afterNote := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Same(t, before.runtime, afterNote.runtime)
		require.Same(t, afterSwitch.listeners, afterNote.listeners)
		require.Equal(t, view.ExposureLocal, afterNote.exposure)
		require.Equal(t, "local", env.store.Exposure("client", "m1"), "ordinary edits must not erase exposure")

		// A port change rebinds, and the new runtime must still be local.
		newPort := freeExposureTCPPort(t)
		rebound := mapping
		rebound.RemotePort = newPort
		rebound.Note = "rebound"
		updateExposureMapping(t, env.manager, sess, 2, rebound)
		afterRebind := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.NotSame(t, before.runtime, afterRebind.runtime)
		require.Equal(t, newPort, afterRebind.assignedPort)
		require.Equal(t, view.ExposureLocal, afterRebind.exposure)
		requireLoopbackListeners(t, afterRebind.listeners)
		require.Equal(t, "local", env.store.Exposure("client", "m1"))

		// Disabling stores the desired local exposure without a listener.
		disabled := rebound
		disabled.Enabled = false
		updateExposureMapping(t, env.manager, sess, 3, disabled)
		afterDisable := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Equal(t, "disabled", afterDisable.status)
		require.Nil(t, afterDisable.listeners)
		require.Equal(t, view.ExposureLocal, afterDisable.exposure)
		require.Equal(t, "local", env.store.Exposure("client", "m1"))

		// A disabled mapping stores the requested exposure immediately; the
		// next enable binds that value.
		disabledView, err := env.manager.SetExposure("client", "m1", view.ExposurePublic)
		require.NoError(t, err)
		require.Equal(t, mappingStatusDisabled, disabledView.Status)
		require.Equal(t, view.ExposurePublic, disabledView.Exposure)
		require.Nil(t, disabledView.UnavailableFamilies)
		require.Equal(t, "public", env.store.Exposure("client", "m1"))
		_, err = env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.NoError(t, err)
		require.Equal(t, "local", env.store.Exposure("client", "m1"))

		// Re-enabling is a bind path and must apply the stored local value.
		updateExposureMapping(t, env.manager, sess, 4, rebound)
		afterEnable := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Equal(t, "running", afterEnable.status)
		require.Equal(t, view.ExposureLocal, afterEnable.exposure)
		requireLoopbackListeners(t, afterEnable.listeners)
		require.Equal(t, "local", env.store.Exposure("client", "m1"))

		// A failed bind retries through the same client-update path and must
		// keep the stored local exposure.
		failedPort := freeExposureTCPPort(t)
		first := config.Mapping{ID: "a", Type: "tcp", RemotePort: failedPort, LocalAddr: "127.0.0.1:9001", Enabled: true}
		second := config.Mapping{ID: "b", Type: "tcp", RemotePort: failedPort, LocalAddr: "127.0.0.1:9002", Enabled: true}
		results := env.manager.register(sess, []config.Mapping{first, second})
		require.Len(t, results, 2)
		require.True(t, results[0].OK)
		require.False(t, results[1].OK)

		failedView, err := env.manager.SetExposure("client", "b", view.ExposureLocal)
		require.NoError(t, err)
		require.Equal(t, view.ExposureLocal, failedView.Exposure)
		failed := snapshotExposureRuntime(t, env.manager, "client", "b")
		require.Equal(t, "failed", failed.status)
		require.Equal(t, view.ExposureLocal, failed.exposure)
		require.Equal(t, "local", env.store.Exposure("client", "b"))

		require.NoError(t, env.manager.removeMapping("client", "a"))
		results, accepted, _ := env.manager.update(sess, 5, []config.Mapping{second})
		require.True(t, accepted)
		require.True(t, findResult(t, results, "b").OK)
		retried := snapshotExposureRuntime(t, env.manager, "client", "b")
		require.Equal(t, "running", retried.status)
		require.Equal(t, view.ExposureLocal, retried.exposure)
		requireLoopbackListeners(t, retried.listeners)
	})

	t.Run("server push keeps local", func(t *testing.T) {
		pair := startTestPair(t, []config.Mapping{
			{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
		})
		defer pair.Stop()

		clientID, base := waitForSettledClient(t, pair.Server)
		switched, err := pair.Server.SetMappingExposure(clientID, "m1", view.ExposureLocal)
		require.NoError(t, err)
		require.Equal(t, view.ExposureLocal, switched.Exposure)
		require.Equal(t, "local", pair.Server.store.Exposure(clientID, "m1"))

		// A keep-path push must not reopen the mapping publicly.
		next := []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Note: "pushed", Enabled: true}}
		revision, err := pair.Server.PushMappings(clientID, base, next)
		require.NoError(t, err)
		require.Equal(t, base+1, revision)
		kept := snapshotTestRuntime(t, pair.Server, clientID, "m1")
		require.Equal(t, view.ExposureLocal, kept.runtime.exposure)
		requireLoopbackListeners(t, kept.runtime.listeners)
		require.Equal(t, "local", pair.Server.store.Exposure(clientID, "m1"))

		// A rebinding push must also apply the stored exposure.
		newPort := freeExposureTCPPort(t)
		next[0].RemotePort = newPort
		next[0].Note = "pushed-rebind"
		revision, err = pair.Server.PushMappings(clientID, revision, next)
		require.NoError(t, err)
		rebound := snapshotTestRuntime(t, pair.Server, clientID, "m1")
		require.Equal(t, newPort, rebound.assignedPort)
		require.Equal(t, view.ExposureLocal, rebound.runtime.exposure)
		requireLoopbackListeners(t, rebound.runtime.listeners)
		require.Equal(t, "local", pair.Server.store.Exposure(clientID, "m1"))
	})
}

func TestMappingExposureDeleteClears(t *testing.T) {
	t.Run("remove mapping clears stored exposure", func(t *testing.T) {
		env := newExposureEnv(t, "")
		sess := newExposureSession("client")
		port1 := freeExposureTCPPort(t)
		port2 := freeExposureTCPPort(t)
		first := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port1, LocalAddr: "127.0.0.1:9001", Enabled: true}
		second := config.Mapping{ID: "m2", Type: "tcp", RemotePort: port2, LocalAddr: "127.0.0.1:9002", Enabled: true}

		results := env.manager.register(sess, []config.Mapping{first, second})
		require.Len(t, results, 2)
		require.True(t, results[0].OK)
		require.True(t, results[1].OK)
		_, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.NoError(t, err)

		require.NoError(t, env.manager.removeMapping("client", "m1"))
		require.Equal(t, "", env.store.Exposure("client", "m1"))
		require.Equal(t, "", env.store.Exposure("client", "m2"), "deleting one mapping must not affect another")

		// Recreating the same ID starts public.
		results, accepted, _ := env.manager.update(sess, 1, []config.Mapping{first, second})
		require.True(t, accepted)
		require.True(t, findResult(t, results, "m1").OK)
		recreated := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Equal(t, view.ExposurePublic, recreated.exposure)
		requirePublicListeners(t, recreated.listeners)
	})

	t.Run("client update removing a mapping clears its exposure", func(t *testing.T) {
		env := newExposureEnv(t, "")
		sess := newExposureSession("client")
		port1 := freeExposureTCPPort(t)
		port2 := freeExposureTCPPort(t)
		first := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port1, LocalAddr: "127.0.0.1:9001", Enabled: true}
		second := config.Mapping{ID: "m2", Type: "tcp", RemotePort: port2, LocalAddr: "127.0.0.1:9002", Enabled: true}

		results := env.manager.register(sess, []config.Mapping{first, second})
		require.Len(t, results, 2)
		require.True(t, results[0].OK)
		require.True(t, results[1].OK)
		_, err := env.manager.SetExposure("client", "m1", view.ExposureLocal)
		require.NoError(t, err)

		updateExposureMapping(t, env.manager, sess, 1, second)
		require.Equal(t, "", env.store.Exposure("client", "m1"))

		results, accepted, _ := env.manager.update(sess, 2, []config.Mapping{first, second})
		require.True(t, accepted)
		require.True(t, findResult(t, results, "m1").OK)
		recreated := snapshotExposureRuntime(t, env.manager, "client", "m1")
		require.Equal(t, view.ExposurePublic, recreated.exposure)
		requirePublicListeners(t, recreated.listeners)
	})
}

func TestMappingExposureConcurrentToggles(t *testing.T) {
	env := newExposureEnv(t, "")
	sess := newExposureSession("client")
	port := freeExposureTCPPort(t)
	mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}
	registerExposureMapping(t, env.manager, sess, mapping)

	const toggles = 20
	var wg sync.WaitGroup
	errs := make(chan error, toggles)
	for i := 0; i < toggles; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			exposure := view.ExposureLocal
			if i%2 == 0 {
				exposure = view.ExposurePublic
			}
			_, err := env.manager.SetExposure("client", "m1", exposure)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	final := snapshotExposureRuntime(t, env.manager, "client", "m1")
	require.Equal(t, "running", final.status)
	stored := view.Exposure(env.store.Exposure("client", "m1"))
	require.Equal(t, final.exposure, stored, "runtime and persisted exposure must agree")
	switch final.exposure {
	case view.ExposureLocal:
		requireLoopbackListeners(t, final.listeners)
	case view.ExposurePublic:
		requirePublicListeners(t, final.listeners)
	default:
		t.Fatalf("unexpected final exposure %q", final.exposure)
	}
}

func TestMappingExposureInvalidStoredFailsClosed(t *testing.T) {
	env := newExposureEnv(t, "")
	port := freeExposureTCPPort(t)
	mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}
	env.store.Upsert(state.ClientState{
		ID:        "client",
		Online:    false,
		Mappings:  []config.Mapping{mapping},
		Exposures: map[string]string{"m1": "bogus"},
	})

	sess := newExposureSession("client")
	results := env.manager.register(sess, []config.Mapping{mapping})
	require.Len(t, results, 1)
	require.False(t, results[0].OK, "an invalid stored exposure must fail closed")
	require.Equal(t, string(control.CodeInternal), results[0].Error)
	require.Equal(t, view.Exposure("bogus"), env.manager.exposureFor("client", "m1"))

	failed := snapshotExposureRuntime(t, env.manager, "client", "m1")
	require.Equal(t, "failed", failed.status)
	require.Nil(t, failed.listeners)
	require.Equal(t, view.Exposure("bogus"), failed.exposure, "an invalid value must not be silently replaced with public")
}

func TestMappingExposureServerWrapper(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	sess := newExposureSession("client")
	port := freeExposureTCPPort(t)
	mapping := config.Mapping{ID: "m1", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true}
	results := srv.mappings.register(sess, []config.Mapping{mapping})
	require.Len(t, results, 1)
	require.True(t, results[0].OK)

	switched, err := srv.SetMappingExposure("client", "m1", view.ExposureLocal)
	require.NoError(t, err)
	require.Equal(t, view.ExposureLocal, switched.Exposure)
	require.Equal(t, "running", switched.Status)

	_, err = srv.SetMappingExposure("missing", "m1", view.ExposureLocal)
	require.ErrorIs(t, err, control.ErrClientNotFound)
}

func TestExposureSwitchErrorUnwrap(t *testing.T) {
	cause := errors.New("bind failed")
	err := &ExposureSwitchError{
		Requested: view.ExposureLocal,
		Exposure:  view.ExposurePublic,
		Err:       cause,
	}
	require.ErrorIs(t, err, cause)
	require.Contains(t, err.Error(), "local")
	require.Contains(t, err.Error(), "public")
	require.True(t, err.ExposureConflict())
	require.False(t, err.Persistence)
	require.Equal(t, view.ExposurePublic, err.ActiveExposure())
	require.False(t, err.MappingClosed())

	closed := &ExposureSwitchError{Requested: view.ExposureLocal, Exposure: view.ExposurePublic, Closed: true, Err: cause}
	require.Contains(t, closed.Error(), "closed")
	require.True(t, closed.MappingClosed())

	persistence := &ExposureSwitchError{
		Requested:   view.ExposureLocal,
		Exposure:    view.ExposurePublic,
		Persistence: true,
		Err:         cause,
	}
	require.False(t, persistence.ExposureConflict())
	require.True(t, persistence.Persistence)
}
