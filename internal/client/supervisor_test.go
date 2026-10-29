package client

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"hy2tun/internal/config"
)

func multiClientConfigForTest() *config.ClientConfig {
	return &config.ClientConfig{
		Web: config.WebConfig{},
		Log: config.LogConfig{Level: "info"},
		Servers: []config.ClientServerConfig{
			{
				Name: "us", Server: "127.0.0.1:24443", Auth: "pw-us",
				Control:     config.ControlConfig{Token: "0123456789abcdef0123456789abcdef"},
				Client:      config.ClientIdentity{Name: "home-us"},
				Credentials: config.CredentialsConfig{Generation: 1},
				Mappings:    []config.Mapping{{ID: "ssh", Type: "tcp", RemotePort: 25022, LocalAddr: "127.0.0.1:22", Enabled: true}},
			},
			{
				Name: "sg", Server: "127.0.0.1:24444", Auth: "pw-sg",
				Control:     config.ControlConfig{Token: "abcdef0123456789abcdef0123456789"},
				Client:      config.ClientIdentity{Name: "home-sg"},
				Credentials: config.CredentialsConfig{Generation: 1},
				Mappings:    []config.Mapping{{ID: "ssh", Type: "tcp", RemotePort: 35022, LocalAddr: "127.0.0.1:22", Enabled: true}},
			},
		},
	}
}

func TestSupervisorCreatesIndependentWorkers(t *testing.T) {
	cfg := multiClientConfigForTest()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()

	require.Len(t, sup.workers, 2)
	require.NotNil(t, sup.workers["us"])
	require.NotNil(t, sup.workers["sg"])
	require.NotEqual(t, sup.workers["us"].cfg.Client.ID, sup.workers["sg"].cfg.Client.ID)
	require.Equal(t, "home-us", sup.workers["us"].cfg.Client.Name)
	require.Equal(t, "home-sg", sup.workers["sg"].cfg.Client.Name)

	servers := cfg.ServerConfigs()
	require.NotEmpty(t, servers[0].Client.ID)
	require.NotEmpty(t, servers[1].Client.ID)
}

func TestSupervisorLegacyConfigCreatesDefaultWorker(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()

	require.Len(t, sup.workers, 1)
	require.NotNil(t, sup.workers["default"])
	require.False(t, cfg.IsMulti())
}

func TestSupervisorPersistsWorkerEditsIntoCorrectServer(t *testing.T) {
	cfg := multiClientConfigForTest()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()

	require.NoError(t, sup.workers["sg"].UpdateName("renamed-sg"))

	data, err := os.ReadFile(cfg.ConfigPath)
	require.NoError(t, err)
	var saved config.ClientConfig
	require.NoError(t, yaml.Unmarshal(data, &saved))
	require.Len(t, saved.Servers, 2)
	require.Equal(t, "home-us", saved.Servers[0].Client.Name)
	require.Equal(t, "renamed-sg", saved.Servers[1].Client.Name)
}

func TestSupervisorPersistsGeneratedLegacyClientIDInLegacyShape(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Client.ID = ""
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	require.NoError(t, config.SaveClient(cfg.ConfigPath, cfg))

	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()

	data, err := os.ReadFile(cfg.ConfigPath)
	require.NoError(t, err)
	require.NotContains(t, string(data), "servers:")
	require.Contains(t, string(data), "id: c-")
}

func TestSupervisorStartStopsAllWorkersOnCancel(t *testing.T) {
	cfg := multiClientConfigForTest()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Start(ctx) }()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not stop after context cancellation")
	}
}

func TestSupervisorExposesServerSummaries(t *testing.T) {
	cfg := multiClientConfigForTest()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()

	summaries := sup.Servers()
	require.Len(t, summaries, 2)
	require.Equal(t, "us", summaries[0].Name)
	api, ok := sup.Server("sg")
	require.True(t, ok)
	require.NotNil(t, api)
	_, ok = sup.Server("missing")
	require.False(t, ok)
}
