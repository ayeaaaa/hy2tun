package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func multiServerTestConfig() *ClientConfig {
	return &ClientConfig{
		Web: WebConfig{Listen: "127.0.0.1:7501", Username: "admin"},
		Log: LogConfig{Level: "info"},
		Servers: []ClientServerConfig{
			{
				Name:   "us",
				Server: "us.example.com:24443",
				Auth:   "us-password",
				Control: ControlConfig{
					Token: strings.Repeat("a", 32),
				},
				Client: ClientIdentity{ID: "c-us", Name: "home-us"},
				Credentials: CredentialsConfig{
					Generation: 1,
				},
				Mappings: []Mapping{{
					ID: "ssh", Type: "tcp", RemotePort: 25022,
					LocalAddr: "127.0.0.1:22", Enabled: true,
				}},
				Proxy: ProxyConfig{Enabled: false, SOCKS5: "127.0.0.1:1080"},
			},
			{
				Name:   "sg",
				Server: "sg.example.com:24443",
				Auth:   "sg-password",
				Control: ControlConfig{
					Token: strings.Repeat("b", 32),
				},
				Client: ClientIdentity{ID: "c-sg", Name: "home-sg"},
				Credentials: CredentialsConfig{
					Generation: 1,
				},
				Mappings: []Mapping{{
					ID: "ssh", Type: "tcp", RemotePort: 35022,
					LocalAddr: "127.0.0.1:22", Enabled: true,
				}},
				Proxy: ProxyConfig{Enabled: true, HTTP: "127.0.0.1:8081"},
			},
		},
	}
}

func TestClientMultiServerConfigValidates(t *testing.T) {
	cfg := multiServerTestConfig()
	require.True(t, cfg.IsMulti())
	require.NoError(t, cfg.Validate())

	servers := cfg.ServerConfigs()
	require.Len(t, servers, 2)
	require.Equal(t, "us", servers[0].Name)
	require.Equal(t, "sg", servers[1].Name)
	require.Equal(t, 25022, servers[0].Mappings[0].RemotePort)
	require.Equal(t, 35022, servers[1].Mappings[0].RemotePort)
}

func TestClientMultiServerRejectsLegacyFields(t *testing.T) {
	cfg := multiServerTestConfig()
	cfg.Server = "legacy.example.com:443"
	require.ErrorContains(t, cfg.Validate(), "legacy")
}

func TestClientMultiServerRejectsDuplicateNames(t *testing.T) {
	cfg := multiServerTestConfig()
	cfg.Servers[1].Name = "us"
	require.ErrorContains(t, cfg.Validate(), "duplicate")
}

func TestClientMultiServerRejectsInvalidServer(t *testing.T) {
	for _, mutate := range []func(*ClientConfig){
		func(cfg *ClientConfig) { cfg.Servers[0].Name = "" },
		func(cfg *ClientConfig) { cfg.Servers[0].Server = "" },
		func(cfg *ClientConfig) { cfg.Servers[0].Auth = "" },
		func(cfg *ClientConfig) { cfg.Servers[0].Control.Token = "short" },
		func(cfg *ClientConfig) {
			cfg.Servers[0].Proxy = ProxyConfig{Enabled: true}
		},
		func(cfg *ClientConfig) {
			cfg.Servers[0].Proxy = ProxyConfig{Enabled: true, SOCKS5: "127.0.0.1:0"}
		},
	} {
		cfg := multiServerTestConfig()
		mutate(cfg)
		require.Error(t, cfg.Validate())
	}
}

func TestClientMultiServerProxyConflicts(t *testing.T) {
	cfg := multiServerTestConfig()
	cfg.Servers[0].Proxy = ProxyConfig{Enabled: true, SOCKS5: "127.0.0.1:1080"}
	cfg.Servers[1].Proxy = ProxyConfig{Enabled: true, HTTP: "127.0.0.1:1080"}
	require.ErrorContains(t, cfg.Validate(), "conflict")

	cfg = multiServerTestConfig()
	cfg.Servers[0].Proxy = ProxyConfig{Enabled: true, SOCKS5: "127.0.0.1:7501"}
	require.ErrorContains(t, cfg.Validate(), "web.listen")
}

func TestClientMultiServerDisabledProxyDoesNotConflict(t *testing.T) {
	cfg := multiServerTestConfig()
	cfg.Servers[0].Proxy = ProxyConfig{Enabled: false, SOCKS5: "127.0.0.1:7501"}
	cfg.Servers[1].Proxy = ProxyConfig{Enabled: false, SOCKS5: "127.0.0.1:7501"}
	require.NoError(t, cfg.Validate())
	require.False(t, cfg.ServerConfigs()[0].Proxy.Active())
}

func TestClientLegacyConfigConvertsToDefaultServer(t *testing.T) {
	cfg := validClientConfig()
	cfg.Proxy = ProxyConfig{SOCKS5: "127.0.0.1:1080"}
	require.False(t, cfg.IsMulti())
	require.NoError(t, cfg.Validate())
	require.True(t, cfg.Proxy.Enabled)

	servers := cfg.ServerConfigs()
	require.Len(t, servers, 1)
	require.Equal(t, "default", servers[0].Name)
	require.Equal(t, cfg.Server, servers[0].Server)
	require.Equal(t, cfg.Auth, servers[0].Auth)
	require.True(t, servers[0].Proxy.Active())

	worker := servers[0].ToClientConfig(cfg.Web, cfg.Log, cfg.ConfigPath)
	require.Equal(t, cfg.Server, worker.Server)
	require.Equal(t, cfg.Auth, worker.Auth)
	require.Equal(t, cfg.Client.ID, worker.Client.ID)
	require.Empty(t, worker.Servers)
}

func TestClientLegacyProxyWithoutAddressesStaysDisabled(t *testing.T) {
	cfg := validClientConfig()
	require.NoError(t, cfg.Validate())
	require.False(t, cfg.Proxy.Enabled)
	require.False(t, cfg.ServerConfigs()[0].Proxy.Active())
}

func TestCheckClientMultiValidatesEveryServerTLS(t *testing.T) {
	cfg := multiServerTestConfig()
	cfg.Servers[1].TLS.CA = "/definitely/missing/ca.pem"
	err := CheckClient(cfg)
	require.ErrorContains(t, err, "/definitely/missing/ca.pem")
}

func TestProxyConflictAllowsDifferentSpecificAddresses(t *testing.T) {
	cfg := multiServerTestConfig()
	cfg.Servers[0].Proxy = ProxyConfig{Enabled: true, SOCKS5: "127.0.0.1:1080"}
	cfg.Servers[1].Proxy = ProxyConfig{Enabled: true, SOCKS5: "127.0.0.2:1080"}
	require.NoError(t, cfg.Validate())
}

func TestProxyListenKeyNormalizesHostCase(t *testing.T) {
	first, err := proxyListenKey("LOCALHOST:1080")
	require.NoError(t, err)
	second, err := proxyListenKey("localhost:1080")
	require.NoError(t, err)
	require.Equal(t, first, second)
}
