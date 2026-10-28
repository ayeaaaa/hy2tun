package server

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/config"
	"hy2tun/internal/state"
	"hy2tun/internal/webui/serverui"
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

func TestSettingsIncludesOfflineClientsAndMasksSecrets(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	now := time.Now().Add(-time.Hour)
	srv.store.Upsert(state.ClientState{
		ID:       "offline-client",
		Name:     "laptop",
		Online:   false,
		LastSeen: now,
	})

	settings := srv.Settings()
	require.Equal(t, "password", settings.AuthType)
	require.Equal(t, serverui.MaskedSecret, settings.Credentials.CurrentAuthMasked)
	require.Equal(t, serverui.MaskedSecret, settings.Credentials.CurrentControlTokenMasked)
	require.NotContains(t, settings.Credentials.CurrentAuthMasked, srv.currentCredentials().Auth)
	require.True(t, settings.Credentials.Rotatable)

	require.Len(t, settings.Clients, 1)
	client := settings.Clients[0]
	require.Equal(t, "offline-client", client.ClientID)
	require.Equal(t, "laptop", client.Name)
	require.False(t, client.Online)
	require.Equal(t, "pending", client.Status)
	require.Equal(t, now.Unix(), client.LastSeen.Unix())

	var readOnly []string
	for _, field := range settings.ReadOnly {
		readOnly = append(readOnly, field.Label)
	}
	require.Contains(t, readOnly, "HY2 监听")
	require.Contains(t, readOnly, "配置文件")
}

func TestUpdateWebConfigWriteFailureKeepsRuntime(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	srv.settingsMu.Lock()
	srv.cfg.ConfigPath = filepath.Join(t.TempDir(), "server.yaml")
	srv.settingsMu.Unlock()

	before := srv.currentConfig().Web
	srv.saveConfig = func(string, *config.ServerConfig) error {
		return errors.New("write failed")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
	require.NoError(t, err)

	err = srv.UpdateWebConfig("root", string(hash))
	require.Error(t, err)
	require.Contains(t, err.Error(), "write failed")
	require.Equal(t, before.Username, srv.currentConfig().Web.Username)
	require.Equal(t, before.PasswordHash, srv.currentConfig().Web.PasswordHash)
}

func TestUpdateWebConfigRejectsInvalidInput(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	require.Error(t, srv.UpdateWebConfig("", "$2a$10$invalid"))
	require.Error(t, srv.UpdateWebConfig("root", "not-a-bcrypt-hash"))
}

func TestSettingsMergesOnlineAndOfflineCredentialStatuses(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 25022, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()
	require.NoError(t, waitFor(t, 5*time.Second, func() bool { return len(pair.Server.Clients()) == 1 }))

	now := time.Now().Add(-30 * time.Minute)
	pair.Server.store.Upsert(state.ClientState{
		ID:       "offline-client",
		Name:     "laptop",
		Online:   false,
		LastSeen: now,
	})

	settings := pair.Server.Settings()
	require.Len(t, settings.Clients, 2)

	online := settings.Clients[0]
	if online.Online == false {
		online, settings.Clients[1] = settings.Clients[1], settings.Clients[0]
	}
	require.True(t, online.Online)
	require.Equal(t, "synced", online.Status)
	require.Equal(t, int64(1), online.Generation)

	var offline serverui.ClientCredentialView
	for _, client := range settings.Clients {
		if !client.Online {
			offline = client
		}
	}
	require.Equal(t, "offline-client", offline.ClientID)
	require.Equal(t, "pending", offline.Status)

	raw, err := json.Marshal(settings)
	require.NoError(t, err)
	require.NotContains(t, string(raw), pair.Server.currentCredentials().Auth)
	require.NotContains(t, string(raw), pair.Server.currentCredentials().ControlToken)
}
