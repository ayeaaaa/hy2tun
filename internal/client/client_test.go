package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
)

func validTestClientConfig() *config.ClientConfig {
	return &config.ClientConfig{
		Server: "127.0.0.1:443",
		Auth:   "pw",
		TLS:    config.ClientTLSConfig{Insecure: true},
		Control: config.ControlConfig{
			Token: "0123456789abcdef0123456789abcdef",
		},
		Client: config.ClientIdentity{Name: "test"},
		Web:    config.WebConfig{Listen: ""},
		Log:    config.LogConfig{Level: "info"},
	}
}

func TestMappingViewsMergesServerResults(t *testing.T) {
	mappings := []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true},
		{ID: "off", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9003", Enabled: false},
	}
	results := map[string]control.Result{
		"tcp1": {ID: "tcp1", OK: true, AssignedPort: 23001},
		"udp1": {ID: "udp1", Error: "port_in_use"},
	}

	views := mappingViews(mappings, results)
	require.Len(t, views, 3)
	require.Equal(t, "running", views[0].Status)
	require.Equal(t, 23001, views[0].AssignedPort)
	require.Equal(t, "failed", views[1].Status)
	require.Equal(t, "port_in_use", views[1].Error)
	require.Equal(t, "disabled", views[2].Status)
}

func TestMappingViewsBeforeHandshakeIsPending(t *testing.T) {
	mappings := []config.Mapping{
		{ID: "m1", Type: "tcp", LocalAddr: "127.0.0.1:9001", Enabled: true},
	}
	views := mappingViews(mappings, nil)
	require.Len(t, views, 1)
	require.Equal(t, "pending", views[0].Status)
}

func TestNewGeneratesClientID(t *testing.T) {
	cfg := validTestClientConfig()
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	require.NotEmpty(t, cfg.Client.ID)
	require.Regexp(t, `^c-[0-9a-f]{16}$`, cfg.Client.ID)
	require.NoError(t, cl.Close())
}

func TestNewRejectsInvalidObfs(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Obfs.Type = "bogus"
	_, err := New(cfg, nil)
	require.ErrorContains(t, err, "unsupported obfs.type")
}

func TestNewRejectsShortSalamanderPassword(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Obfs.Type = "salamander"
	cfg.Obfs.Salamander.Password = "abc"
	_, err := New(cfg, nil)
	require.Error(t, err)
}

func TestWaitReadyAfterClose(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	require.NoError(t, cl.Close())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.Error(t, cl.WaitReady(ctx))
}

func TestParsePin(t *testing.T) {
	valid := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	pin, err := parsePin(valid)
	require.NoError(t, err)
	require.Len(t, pin, 32)

	withPrefix, err := parsePin("sha256:" + valid)
	require.NoError(t, err)
	require.Equal(t, pin, withPrefix)

	withColons, err := parsePin("01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef")
	require.NoError(t, err)
	require.Equal(t, pin, withColons)

	_, err = parsePin("not-a-pin")
	require.Error(t, err)
}
