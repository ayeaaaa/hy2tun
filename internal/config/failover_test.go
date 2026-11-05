package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func writeFailoverClient(t *testing.T, extra string) *ClientConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.yaml")
	body := `server: "127.0.0.1:443"
auth: "pw"
control:
  token: "0123456789abcdef0123456789abcdef"
` + extra
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	cfg, err := LoadClient(path)
	require.NoError(t, err)
	return cfg
}

func TestFailoverConfigMiddleProfile(t *testing.T) {
	cfg := writeFailoverClient(t, `failover:
  connect_timeout: "8s"
  heartbeat_interval: "5s"
  heartbeat_timeout: "20s"
  probe_interval: "15s"
  min_tcp_hold: "30s"
  recovery_successes: 3
`)
	require.NotNil(t, cfg.Failover)
	require.Equal(t, 8*time.Second, time.Duration(cfg.Failover.ConnectTimeout))
	require.Equal(t, 5*time.Second, time.Duration(cfg.Failover.HeartbeatInterval))
	require.Equal(t, 20*time.Second, time.Duration(cfg.Failover.HeartbeatTimeout))
	require.Equal(t, 15*time.Second, time.Duration(cfg.Failover.ProbeInterval))
	require.Equal(t, 30*time.Second, time.Duration(cfg.Failover.MinTCPHold))
	require.Equal(t, 3, cfg.Failover.RecoverySuccesses)

	effective := cfg.Failover.Effective()
	require.Equal(t, 8*time.Second, time.Duration(effective.ConnectTimeout))
	require.Equal(t, 5*time.Second, time.Duration(effective.HeartbeatInterval))
	require.Equal(t, 20*time.Second, time.Duration(effective.HeartbeatTimeout))
	require.Equal(t, 15*time.Second, time.Duration(effective.ProbeInterval))
	require.Equal(t, 30*time.Second, time.Duration(effective.MinTCPHold))
	require.Equal(t, 3, effective.RecoverySuccesses)
}

func TestFailoverConfigDefaults(t *testing.T) {
	cfg := writeFailoverClient(t, "")
	require.Nil(t, cfg.Failover)
	effective := FailoverConfig{}.Effective()
	require.Equal(t, DefaultFailoverConnectTimeout, time.Duration(effective.ConnectTimeout))
	require.Equal(t, DefaultFailoverHeartbeatInterval, time.Duration(effective.HeartbeatInterval))
	require.Equal(t, DefaultFailoverHeartbeatTimeout, time.Duration(effective.HeartbeatTimeout))
	require.Equal(t, DefaultFailoverProbeInterval, time.Duration(effective.ProbeInterval))
	require.Equal(t, DefaultFailoverMinTCPHold, time.Duration(effective.MinTCPHold))
	require.Equal(t, DefaultFailoverRecoverySuccesses, effective.RecoverySuccesses)
}

func TestFailoverConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		f    FailoverConfig
		want string
	}{
		{"negative connect", FailoverConfig{ConnectTimeout: Duration(-time.Second)}, "connect_timeout"},
		{"too short connect", FailoverConfig{ConnectTimeout: Duration(500 * time.Millisecond)}, "connect_timeout"},
		{"too short heartbeat", FailoverConfig{HeartbeatInterval: Duration(500 * time.Millisecond)}, "heartbeat_interval"},
		{"timeout not greater than interval", FailoverConfig{
			HeartbeatInterval: Duration(10 * time.Second),
			HeartbeatTimeout:  Duration(10 * time.Second),
		}, "heartbeat_timeout"},
		{"too short probe", FailoverConfig{ProbeInterval: Duration(500 * time.Millisecond)}, "probe_interval"},
		{"too short hold", FailoverConfig{MinTCPHold: Duration(500 * time.Millisecond)}, "min_tcp_hold"},
		{"negative successes", FailoverConfig{RecoverySuccesses: -1}, "recovery_successes"},
		{"too many successes", FailoverConfig{RecoverySuccesses: 11}, "recovery_successes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, tc.f.Validate("failover"), tc.want)
		})
	}
}

func TestMultiServerFailoverIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	body := `servers:
  - name: "us"
    server: "127.0.0.1:443"
    auth: "pw-us"
    control:
      token: "0123456789abcdef0123456789abcdef"
    failover:
      connect_timeout: "8s"
      heartbeat_interval: "5s"
      heartbeat_timeout: "20s"
      probe_interval: "15s"
      min_tcp_hold: "30s"
      recovery_successes: 3
  - name: "sg"
    server: "127.0.0.1:444"
    auth: "pw-sg"
    control:
      token: "abcdef0123456789abcdef0123456789"
    failover:
      connect_timeout: "12s"
      recovery_successes: 5
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	cfg, err := LoadClient(path)
	require.NoError(t, err)

	servers := cfg.ServerConfigs()
	require.Len(t, servers, 2)
	require.Equal(t, 8*time.Second, time.Duration(servers[0].Failover.ConnectTimeout))
	require.Equal(t, 3, servers[0].Failover.RecoverySuccesses)
	require.Equal(t, 12*time.Second, time.Duration(servers[1].Failover.ConnectTimeout))
	require.Equal(t, 5, servers[1].Failover.RecoverySuccesses)

	worker := servers[1].ToClientConfig(WebConfig{}, LogConfig{}, "")
	require.Equal(t, 12*time.Second, time.Duration(worker.Failover.ConnectTimeout))
	require.Equal(t, 5, worker.Failover.RecoverySuccesses)
}

func TestFailoverIsKnownBasicKeyAndAdvanced(t *testing.T) {
	require.True(t, isKnownTopLevelKey("failover"))
	require.True(t, isAdvancedConfig(&ClientConfig{Failover: &FailoverConfig{ConnectTimeout: Duration(time.Second)}}))

	path := writeFailoverClientPath(t, `failover:
  connect_timeout: "8s"
`)
	got, err := GetBasic(path)
	require.NoError(t, err)
	require.False(t, got.UnknownFields)
	require.True(t, got.Advanced)
}

func writeFailoverClientPath(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.yaml")
	body := `server: "127.0.0.1:443"
auth: "pw"
control:
  token: "0123456789abcdef0123456789abcdef"
` + extra
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	return path
}

func TestSetBasicPreservesFailover(t *testing.T) {
	path := writeFailoverClientPath(t, `failover:
  connect_timeout: "8s"
  heartbeat_interval: "5s"
  heartbeat_timeout: "20s"
  probe_interval: "15s"
  min_tcp_hold: "30s"
  recovery_successes: 3
`)
	got, err := GetBasic(path)
	require.NoError(t, err)
	_, err = SetBasic(path, &BasicUpdate{
		ExpectedRevision: got.Revision,
		Server:           basicStringPtr("127.0.0.1:443"),
		ClientName:       basicStringPtr("router"),
		Mappings:         basicMappingsPtr(nil),
		WebListen:        basicStringPtr(""),
		WebUsername:      basicStringPtr(""),
	}, SetBasicOptions{})
	require.NoError(t, err)

	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Failover)
	require.Equal(t, 8*time.Second, time.Duration(cfg.Failover.ConnectTimeout))
	require.Equal(t, 5*time.Second, time.Duration(cfg.Failover.HeartbeatInterval))
	require.Equal(t, 20*time.Second, time.Duration(cfg.Failover.HeartbeatTimeout))
	require.Equal(t, 15*time.Second, time.Duration(cfg.Failover.ProbeInterval))
	require.Equal(t, 30*time.Second, time.Duration(cfg.Failover.MinTCPHold))
	require.Equal(t, 3, cfg.Failover.RecoverySuccesses)
}

func TestMultiServerRejectsTopLevelFailover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	body := `failover:
  connect_timeout: "8s"
servers:
  - name: "us"
    server: "127.0.0.1:443"
    auth: "pw"
    control:
      token: "0123456789abcdef0123456789abcdef"
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	_, err := LoadClient(path)
	require.ErrorContains(t, err, "servers cannot be combined")
}
