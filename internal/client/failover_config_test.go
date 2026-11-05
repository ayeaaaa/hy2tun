package client

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
)

func TestClientFailoverPolicyFromConfig(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Failover = &config.FailoverConfig{
		ConnectTimeout:    config.Duration(8 * time.Second),
		HeartbeatInterval: config.Duration(5 * time.Second),
		HeartbeatTimeout:  config.Duration(20 * time.Second),
		ProbeInterval:     config.Duration(15 * time.Second),
		MinTCPHold:        config.Duration(30 * time.Second),
		RecoverySuccesses: 3,
	}
	cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{})
	require.NoError(t, err)
	defer cl.Close()

	require.Equal(t, FailoverPolicy{
		ConnectTimeout:    8 * time.Second,
		HeartbeatInterval: 5 * time.Second,
		HeartbeatTimeout:  20 * time.Second,
		ProbeInterval:     15 * time.Second,
		MinTCPHold:        30 * time.Second,
		RecoverySuccesses: 3,
	}, cl.runtime.Policy)
}

func TestSupervisorFailoverPerServer(t *testing.T) {
	path := t.TempDir() + "/client.yaml"
	body := `servers:
  - name: "us"
    server: "127.0.0.1:443"
    auth: "pw-us"
    control:
      token: "0123456789abcdef0123456789abcdef"
    failover:
      connect_timeout: "8s"
  - name: "sg"
    server: "127.0.0.1:444"
    auth: "pw-sg"
    control:
      token: "abcdef0123456789abcdef0123456789"
    failover:
      connect_timeout: "12s"
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	cfg, err := config.LoadClient(path)
	require.NoError(t, err)
	sup, err := NewSupervisor(cfg, nil)
	require.NoError(t, err)
	defer sup.Close()

	usAPI, ok := sup.Server("us")
	require.True(t, ok)
	us := usAPI.(*Client)
	require.Equal(t, 8*time.Second, us.runtime.Policy.ConnectTimeout)

	sgAPI, ok := sup.Server("sg")
	require.True(t, ok)
	sg := sgAPI.(*Client)
	require.Equal(t, 12*time.Second, sg.runtime.Policy.ConnectTimeout)
}

func TestClientFailoverPolicyDefaultsAndOverride(t *testing.T) {
	cfg := validTestClientConfig()
	cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{})
	require.NoError(t, err)
	require.Equal(t, DefaultFailoverPolicy(), cl.runtime.Policy)
	cl.Close()

	override := FailoverPolicy{
		ConnectTimeout:    3 * time.Second,
		HeartbeatInterval: 4 * time.Second,
		HeartbeatTimeout:  12 * time.Second,
		ProbeInterval:     7 * time.Second,
		MinTCPHold:        9 * time.Second,
		RecoverySuccesses: 2,
	}
	cl, err = NewWithRuntime(cfg, nil, RuntimeOptions{Policy: override})
	require.NoError(t, err)
	defer cl.Close()
	require.Equal(t, override, cl.runtime.Policy)
}
