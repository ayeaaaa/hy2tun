package server

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/state"
)

func TestHotPortPolicyUpdate(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 25022, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()
	require.NoError(t, waitFor(t, 5*time.Second, func() bool { return len(pair.Server.Clients()) == 1 }))

	require.NoError(t, pair.Server.UpdatePorts([]string{"25022", "25023"}, nil, "25000-25099", time.Hour))
	// Existing mapping still runs.
	require.Equal(t, "running", pair.Server.Clients()[0].Mappings[0].Status)

	// New disallowed port is rejected on the next update.
	_, err := pair.Server.PushMappings(pair.Server.Clients()[0].ID, pair.Server.Clients()[0].Revision, []config.Mapping{
		{ID: "tcp2", Type: "tcp", RemotePort: 25024, LocalAddr: "127.0.0.1:9002", Enabled: true},
	})
	var pushErr *PushError
	require.ErrorAs(t, err, &pushErr)
	require.Equal(t, "port_not_allowed", pushErr.Results[0].Error)
}

func TestHotOfflineReserve(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	now := time.Now()
	srv.store.Upsert(state.ClientState{
		ID:     "client-offline",
		Online: true,
		Mappings: []config.Mapping{
			{ID: "tcp1", Type: "tcp", RemotePort: 25022, LocalAddr: "127.0.0.1:9001", Enabled: true},
		},
	})

	require.NoError(t, srv.UpdatePorts(nil, nil, "25000-25099", 2*time.Hour))
	srv.store.SetOnline("client-offline", false, now)

	require.True(t, srv.store.Reserved("client-offline", 25022, now.Add(time.Hour)),
		"the new offline reserve must apply to later offline transitions")
	require.False(t, srv.store.Reserved("client-offline", 25022, now.Add(3*time.Hour)))
}

func TestUpdatePortsInvalidRangeKeepsRuntime(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	beforePolicy := srv.mappings.policy.Load()
	before := srv.currentConfig()

	err := srv.UpdatePorts([]string{"not-a-port"}, nil, "25000-25099", time.Hour)
	require.Error(t, err)

	require.Same(t, beforePolicy, srv.mappings.policy.Load())
	require.Equal(t, append([]string(nil), before.Ports.Allow...), srv.currentConfig().Ports.Allow)
}

func TestTrustProxyToggle(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	require.NoError(t, srv.SetTrustProxy(true))
	require.True(t, srv.trustProxy.Load())
	require.True(t, srv.ConfigSummary().TrustProxy)

	require.NoError(t, srv.SetTrustProxy(false))
	require.False(t, srv.trustProxy.Load())
	require.False(t, srv.ConfigSummary().TrustProxy)
}

func TestUpdatePortsWriteFailureKeepsRuntime(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	srv.settingsMu.Lock()
	srv.cfg.ConfigPath = filepath.Join(t.TempDir(), "server.yaml")
	srv.settingsMu.Unlock()

	beforePolicy := srv.mappings.policy.Load()
	before := srv.currentConfig()
	beforeAllow := append([]string(nil), before.Ports.Allow...)
	beforeReserve := time.Duration(before.Ports.OfflineReserve)

	srv.saveConfig = func(string, *config.ServerConfig) error {
		return errors.New("write failed")
	}
	err := srv.UpdatePorts([]string{"25022"}, nil, "25000-25099", 2*time.Hour)
	require.Error(t, err)
	require.Contains(t, err.Error(), "write failed")

	require.Same(t, beforePolicy, srv.mappings.policy.Load(),
		"a failed config write must not swap the port policy")
	require.Equal(t, beforeAllow, srv.currentConfig().Ports.Allow)
	require.Equal(t, beforeReserve, time.Duration(srv.currentConfig().Ports.OfflineReserve))

	// The store still uses the old offline reserve.
	now := time.Now()
	srv.store.Upsert(state.ClientState{
		ID:     "write-failure",
		Online: true,
		Mappings: []config.Mapping{
			{ID: "tcp1", Type: "tcp", RemotePort: 25022, LocalAddr: "127.0.0.1:9001", Enabled: true},
		},
	})
	srv.store.SetOnline("write-failure", false, now)
	require.True(t, srv.store.Reserved("write-failure", 25022, now.Add(beforeReserve-time.Hour)))
	require.False(t, srv.store.Reserved("write-failure", 25022, now.Add(beforeReserve+time.Hour)))
}

func TestSetTrustProxyWriteFailureKeepsRuntime(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	srv.settingsMu.Lock()
	srv.cfg.ConfigPath = filepath.Join(t.TempDir(), "server.yaml")
	srv.settingsMu.Unlock()

	srv.saveConfig = func(string, *config.ServerConfig) error {
		return errors.New("write failed")
	}
	require.Error(t, srv.SetTrustProxy(true))
	require.False(t, srv.trustProxy.Load())
	require.False(t, srv.ConfigSummary().TrustProxy)
	require.False(t, srv.currentConfig().Web.TrustProxy)
}
