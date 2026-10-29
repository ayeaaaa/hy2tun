package client

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
)

func TestSupervisorUpdateProxyIndependentServers(t *testing.T) {
	cfg := multiClientConfigForTest()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()

	first := freeTCPAddress(t)
	second := freeTCPAddress(t)
	require.NoError(t, sup.UpdateProxy("us", config.ProxyConfig{Enabled: true, SOCKS5: first}))
	require.NoError(t, sup.UpdateProxy("sg", config.ProxyConfig{Enabled: true, SOCKS5: second}))
	require.True(t, sup.workers["us"].ProxyState().Enabled)
	require.True(t, sup.workers["sg"].ProxyState().Enabled)

	require.NoError(t, sup.UpdateProxy("us", config.ProxyConfig{Enabled: false, SOCKS5: first}))
	require.False(t, sup.workers["us"].ProxyState().Enabled)
	require.True(t, sup.workers["sg"].ProxyState().Enabled)
}

func TestSupervisorUpdateProxyRejectsConflict(t *testing.T) {
	cfg := multiClientConfigForTest()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()

	addr := freeTCPAddress(t)
	require.NoError(t, sup.UpdateProxy("us", config.ProxyConfig{Enabled: true, SOCKS5: addr}))
	err = sup.UpdateProxy("sg", config.ProxyConfig{Enabled: true, SOCKS5: addr})
	require.ErrorContains(t, err, "conflict")
	require.False(t, sup.workers["sg"].ProxyState().Enabled)
	require.True(t, sup.workers["us"].ProxyState().Enabled)
}

func TestSupervisorUpdateProxyBindFailureKeepsOldState(t *testing.T) {
	cfg := multiClientConfigForTest()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()

	oldAddr := freeTCPAddress(t)
	require.NoError(t, sup.UpdateProxy("us", config.ProxyConfig{Enabled: true, SOCKS5: oldAddr}))
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()

	err = sup.UpdateProxy("us", config.ProxyConfig{Enabled: true, SOCKS5: occupied.Addr().String()})
	require.Error(t, err)
	state := sup.workers["us"].ProxyState()
	require.True(t, state.Enabled)
	require.Equal(t, oldAddr, state.SOCKS5)
}

func TestSupervisorUpdateProxySaveFailureRestoresOldState(t *testing.T) {
	cfg := multiClientConfigForTest()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	require.NoError(t, os.Mkdir(stateDir, 0700))
	cfg.ConfigPath = filepath.Join(stateDir, "client.yaml")
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()

	oldAddr := freeTCPAddress(t)
	require.NoError(t, sup.UpdateProxy("us", config.ProxyConfig{Enabled: true, SOCKS5: oldAddr}))

	// Replace the config directory with a file so the next save fails.
	require.NoError(t, os.RemoveAll(stateDir))
	require.NoError(t, os.WriteFile(stateDir, []byte("not a directory"), 0600))

	newAddr := freeTCPAddress(t)
	err = sup.UpdateProxy("us", config.ProxyConfig{Enabled: true, SOCKS5: newAddr})
	require.Error(t, err)
	state := sup.workers["us"].ProxyState()
	require.True(t, state.Enabled)
	require.Equal(t, oldAddr, state.SOCKS5)
}

func TestSupervisorStartupProxyBindFailureDoesNotFailWorker(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()

	cfg := multiClientConfigForTest()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	cfg.Servers[0].Proxy = config.ProxyConfig{Enabled: true, SOCKS5: occupied.Addr().String()}
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()
	sup.applyStartupProxies()

	require.NotNil(t, sup.workers["us"])
	require.NotEmpty(t, sup.workers["us"].ProxyState().Error)
	require.False(t, sup.workers["us"].ProxyState().Enabled)
}

func TestSupervisorUpdateProxyPreservesAuth(t *testing.T) {
	cfg := multiClientConfigForTest()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	cfg.Servers[0].Proxy = config.ProxyConfig{
		Enabled: false, SOCKS5: "127.0.0.1:1080", SOCKS5Auth: "user:secret",
		HTTPAuth: "http:secret",
	}
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()

	addr := freeTCPAddress(t)
	require.NoError(t, sup.UpdateProxy("us", config.ProxyConfig{Enabled: true, SOCKS5: addr}))
	require.Equal(t, "user:secret", sup.cfg.Servers[0].Proxy.SOCKS5Auth)
	require.Equal(t, "http:secret", sup.cfg.Servers[0].Proxy.HTTPAuth)
}
