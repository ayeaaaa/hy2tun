package client

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
)

func freeTCPAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func TestApplyProxyDisabledStartsNoListener(t *testing.T) {
	cfg := validTestClientConfig()
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer cl.Close()

	require.NoError(t, cl.ApplyProxy(config.ProxyConfig{
		Enabled: false,
		SOCKS5:  freeTCPAddress(t),
	}))
	state := cl.ProxyState()
	require.False(t, state.Enabled)
	require.Empty(t, state.Error)
}

func TestApplyProxyEnabledBindsAndReportsState(t *testing.T) {
	cfg := validTestClientConfig()
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer cl.Close()

	socks := freeTCPAddress(t)
	httpAddr := freeTCPAddress(t)
	require.NoError(t, cl.ApplyProxy(config.ProxyConfig{
		Enabled: true,
		SOCKS5:  socks,
		HTTP:    httpAddr,
	}))
	state := cl.ProxyState()
	require.True(t, state.Enabled)
	require.Equal(t, socks, state.SOCKS5)
	require.Equal(t, httpAddr, state.HTTP)
	require.Empty(t, state.Error)
}

func TestApplyProxyBindFailureKeepsPriorState(t *testing.T) {
	cfg := validTestClientConfig()
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer cl.Close()

	oldAddr := freeTCPAddress(t)
	require.NoError(t, cl.ApplyProxy(config.ProxyConfig{Enabled: true, SOCKS5: oldAddr}))

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	err = cl.ApplyProxy(config.ProxyConfig{Enabled: true, SOCKS5: occupied.Addr().String()})
	require.Error(t, err)
	state := cl.ProxyState()
	require.True(t, state.Enabled)
	require.Equal(t, oldAddr, state.SOCKS5)
	require.NotEmpty(t, state.Error)
}

func TestApplyProxyChangingAddressRebinds(t *testing.T) {
	cfg := validTestClientConfig()
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer cl.Close()

	first := freeTCPAddress(t)
	second := freeTCPAddress(t)
	require.NoError(t, cl.ApplyProxy(config.ProxyConfig{Enabled: true, SOCKS5: first}))
	require.NoError(t, cl.ApplyProxy(config.ProxyConfig{Enabled: true, SOCKS5: second}))
	require.Equal(t, second, cl.ProxyState().SOCKS5)
}

func TestWorkerPersistReceivesEditedConfig(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.ConfigPath = t.TempDir() + "/client.yaml"
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer cl.Close()

	var got *config.ClientConfig
	cl.SetPersist(func(next *config.ClientConfig) error {
		got = next
		return nil
	})
	require.NoError(t, cl.UpdateName("renamed"))
	require.NotNil(t, got)
	require.Equal(t, "renamed", got.Client.Name)
}

func TestApplyProxyPreservesUnchangedListener(t *testing.T) {
	cfg := validTestClientConfig()
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer cl.Close()

	socks := freeTCPAddress(t)
	httpAddr := freeTCPAddress(t)
	require.NoError(t, cl.ApplyProxy(config.ProxyConfig{Enabled: true, SOCKS5: socks}))
	cl.proxyMu.Lock()
	original := cl.proxies[0]
	cl.proxyMu.Unlock()

	require.NoError(t, cl.ApplyProxy(config.ProxyConfig{Enabled: true, SOCKS5: socks, HTTP: httpAddr}))
	cl.proxyMu.Lock()
	defer cl.proxyMu.Unlock()
	require.Same(t, original, cl.proxies[0])
}
