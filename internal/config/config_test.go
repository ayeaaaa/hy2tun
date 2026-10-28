package config

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

func validServerConfig() *ServerConfig {
	return &ServerConfig{
		Listen: ":443",
		Auth: AuthConfig{
			Type:     "password",
			Password: "pw",
		},
		TLS: TLSConfig{
			Cert: "cert.pem",
			Key:  "key.pem",
		},
		Control: ControlConfig{Token: strings.Repeat("a", 32)},
		Ports: PortsConfig{
			Allow:          []string{"2222", "8000-8100"},
			AutoRange:      "20000-29999",
			OfflineReserve: Duration(24 * time.Hour),
		},
		Web: WebConfig{
			Listen:   "127.0.0.1:7500",
			Username: "admin",
		},
	}
}

func validClientConfig() *ClientConfig {
	return &ClientConfig{
		Server:  "127.0.0.1:443",
		Auth:    "pw",
		Control: ControlConfig{Token: strings.Repeat("b", 32)},
		Client: ClientIdentity{
			ID:       "client-1",
			Name:     "home",
			Revision: 1,
		},
		Mappings: []Mapping{{
			ID:         "m1",
			Type:       "tcp",
			RemotePort: 2222,
			LocalAddr:  "127.0.0.1:22",
			Enabled:    true,
		}},
	}
}

func TestLoadServerConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
listen: ":443"
auth:
  type: password
  password: "pw"
tls:
  cert: "cert.pem"
  key: "key.pem"
control:
  token: "0123456789abcdef0123456789abcdef"
ports:
  allow: ["2222", "8000-8100"]
  deny: ["8001"]
  auto_range: "20000-29999"
  offline_reserve: "24h"
web:
  listen: "127.0.0.1:7500"
  username: "admin"
`), 0600))

	cfg, err := LoadServer(path)
	require.NoError(t, err)
	require.Equal(t, ":443", cfg.Listen)
	require.Equal(t, "password", cfg.Auth.Type)
	require.Equal(t, []string{"2222", "8000-8100"}, cfg.Ports.Allow)
	require.Equal(t, "20000-29999", cfg.Ports.AutoRange)
	require.Equal(t, 24*time.Hour, time.Duration(cfg.Ports.OfflineReserve))
	require.Equal(t, "info", cfg.Log.Level)
	require.Equal(t, int64(1), cfg.Credentials.Generation)
}

func TestLoadClientConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
server: "127.0.0.1:443"
auth: "pw"
tls:
  insecure: true
  sni: "example.com"
control:
  token: "0123456789abcdef0123456789abcdef"
client:
  id: "client-1"
  name: "home"
  revision: 2
mappings:
  - id: "m1"
    type: tcp
    remote_port: 2222
    local_addr: "127.0.0.1:22"
    note: "ssh"
    enabled: true
  - id: "m2"
    type: udp
    remote_port: 0
    local_addr: "127.0.0.1:51820"
    enabled: false
web:
  listen: "127.0.0.1:7501"
proxy:
  socks5: "127.0.0.1:1080"
`), 0600))

	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:443", cfg.Server)
	require.Equal(t, "pw", cfg.Auth)
	require.True(t, cfg.TLS.Insecure)
	require.Equal(t, "example.com", cfg.TLS.SNI)
	require.Equal(t, int64(2), cfg.Client.Revision)
	require.Len(t, cfg.Mappings, 2)
	require.Equal(t, "ssh", cfg.Mappings[0].Note)
	require.False(t, cfg.Mappings[1].Enabled)
	require.Equal(t, "info", cfg.Log.Level)
	require.Equal(t, int64(1), cfg.Credentials.Generation)
}

func TestBandwidthParsing(t *testing.T) {
	var b Bandwidth
	require.NoError(t, yaml.Unmarshal([]byte(`"100 mbps"`), &b))
	require.Equal(t, uint64(12_500_000), uint64(b))
}

func TestBandwidthParsingAllForms(t *testing.T) {
	tests := []struct {
		in   string
		want uint64
	}{
		{`"500 kbps"`, 62_500},
		{`"1 gbps"`, 125_000_000},
		{`"8 mb"`, 8 * 1024 * 1024},
		{`12500000`, 12_500_000},
	}
	for _, tt := range tests {
		var b Bandwidth
		require.NoError(t, yaml.Unmarshal([]byte(tt.in), &b), tt.in)
		require.Equal(t, tt.want, uint64(b), tt.in)
	}
}

func TestBandwidthRejectsInvalid(t *testing.T) {
	for _, in := range []string{`"fast"`, `"100 xbps"`, `"-1 mbps"`, `"-5"`} {
		var b Bandwidth
		require.Error(t, yaml.Unmarshal([]byte(in), &b), in)
	}
}

func TestQUICConfigParsesByteSizes(t *testing.T) {
	var q QUICConfig
	require.NoError(t, yaml.Unmarshal([]byte(`
initial_stream_receive_window: "8 mb"
max_stream_receive_window: "8 mb"
initial_connection_receive_window: "20 mb"
max_connection_receive_window: "20 mb"
max_idle_timeout: "30s"
max_incoming_streams: 4096
`), &q))
	require.Equal(t, uint64(8*1024*1024), q.InitialStreamReceiveWindow)
	require.Equal(t, uint64(20*1024*1024), q.InitialConnectionReceiveWindow)
	require.Equal(t, 30*time.Second, time.Duration(q.MaxIdleTimeout))
	require.Equal(t, int64(4096), q.MaxIncomingStreams)
}

func TestDurationParsing(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want time.Duration
	}{
		{`"30s"`, 30 * time.Second},
		{`"24h"`, 24 * time.Hour},
	} {
		var d Duration
		require.NoError(t, yaml.Unmarshal([]byte(tt.in), &d), tt.in)
		require.Equal(t, tt.want, time.Duration(d), tt.in)
	}

	var d Duration
	require.Error(t, yaml.Unmarshal([]byte(`"later"`), &d))
	require.Error(t, yaml.Unmarshal([]byte(`30`), &d))
}

func TestServerConfigRejectsShortToken(t *testing.T) {
	cfg := validServerConfig()
	cfg.Control.Token = "short"
	require.ErrorContains(t, cfg.Validate(), "control.token")
}

func TestServerConfigRejectsBadPortRange(t *testing.T) {
	cfg := validServerConfig()
	cfg.Ports.Allow = []string{"9000-8000"}
	require.ErrorContains(t, cfg.Validate(), "ports.allow")
}

func TestServerConfigRejectsBadDenyRange(t *testing.T) {
	cfg := validServerConfig()
	cfg.Ports.Deny = []string{"70000"}
	require.ErrorContains(t, cfg.Validate(), "ports.deny")
}

func TestServerConfigRejectsNonNumericAllowRange(t *testing.T) {
	cfg := validServerConfig()
	cfg.Ports.Allow = []string{"http"}
	require.ErrorContains(t, cfg.Validate(), "ports.allow")
}

func TestServerConfigRejectsBadAutoRange(t *testing.T) {
	cfg := validServerConfig()
	cfg.Ports.AutoRange = "not-a-range"
	require.ErrorContains(t, cfg.Validate(), "ports.auto_range")
}

func TestServerConfigRejectsUnpairedTLS(t *testing.T) {
	cfg := validServerConfig()
	cfg.TLS.Key = ""
	require.ErrorContains(t, cfg.Validate(), "tls.cert and tls.key")
}

func TestServerConfigRejectsBadListen(t *testing.T) {
	cfg := validServerConfig()
	cfg.Listen = "443"
	require.ErrorContains(t, cfg.Validate(), "listen")
}

func TestServerConfigRejectsBadAuth(t *testing.T) {
	cfg := validServerConfig()
	cfg.Auth.Type = "token"
	require.ErrorContains(t, cfg.Validate(), "auth.type")

	cfg = validServerConfig()
	cfg.Auth.Type = "userpass"
	cfg.Auth.Users = nil
	require.ErrorContains(t, cfg.Validate(), "auth.users")
}

func TestServerConfigAllowsEmptyWebListen(t *testing.T) {
	cfg := validServerConfig()
	cfg.Web.Listen = ""
	require.NoError(t, cfg.Validate())
}

func TestClientConfigRejectsDuplicateMappingIDs(t *testing.T) {
	cfg := validClientConfig()
	cfg.Mappings = append(cfg.Mappings, Mapping{
		ID: "m1", Type: "udp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true,
	})
	require.ErrorContains(t, cfg.Validate(), "duplicated")
}

func TestClientConfigRejectsBadLocalAddr(t *testing.T) {
	cfg := validClientConfig()
	cfg.Mappings[0].LocalAddr = "127.0.0.1"
	require.ErrorContains(t, cfg.Validate(), "local_addr")
}

func TestClientConfigRejectsBadMappingType(t *testing.T) {
	cfg := validClientConfig()
	cfg.Mappings[0].Type = "http"
	require.ErrorContains(t, cfg.Validate(), "mappings[0].type")
}

func TestClientConfigRejectsMissingToken(t *testing.T) {
	cfg := validClientConfig()
	cfg.Control.Token = ""
	require.ErrorContains(t, cfg.Validate(), "control.token")
}

func TestClientConfigGeneratesMissingMappingID(t *testing.T) {
	cfg := validClientConfig()
	cfg.Mappings[0].ID = ""
	require.NoError(t, cfg.Validate())
	require.NotEmpty(t, cfg.Mappings[0].ID)
}

func TestSaveServerWritesModeAndBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yaml")

	cfg := validServerConfig()
	require.NoError(t, SaveServer(path, cfg))

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	_, err = os.Stat(path + ".bak")
	require.True(t, os.IsNotExist(err), "first save must not create a backup")

	cfg.Listen = ":8443"
	require.NoError(t, SaveServer(path, cfg))

	info, err = os.Stat(path + ".bak")
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	backup, err := LoadServer(path + ".bak")
	require.NoError(t, err)
	require.Equal(t, ":443", backup.Listen)

	loaded, err := LoadServer(path)
	require.NoError(t, err)
	require.Equal(t, ":8443", loaded.Listen)
}

func TestSaveClientRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.yaml")

	cfg := validClientConfig()
	cfg.Mappings[0].Note = "ssh"
	require.NoError(t, SaveClient(path, cfg))

	loaded, err := LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, cfg.Client.ID, loaded.Client.ID)
	require.Equal(t, cfg.Mappings, loaded.Mappings)
	require.Equal(t, "info", loaded.Log.Level)
}

func TestEnsureWebPassword(t *testing.T) {
	web := &WebConfig{}
	pw, generated, err := EnsureWebPassword(web)
	require.NoError(t, err)
	require.True(t, generated)
	require.Len(t, pw, 20)
	require.Equal(t, "admin", web.Username)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(web.PasswordHash), []byte(pw)))
}

func TestEnsureWebPasswordKeepsExistingHash(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("known"), bcrypt.MinCost)
	require.NoError(t, err)
	web := &WebConfig{Username: "operator", PasswordHash: string(hash)}

	pw, generated, err := EnsureWebPassword(web)
	require.NoError(t, err)
	require.False(t, generated)
	require.Empty(t, pw)
	require.Equal(t, string(hash), web.PasswordHash)
	require.Equal(t, "operator", web.Username)
}

func TestEnsureWebPasswordRejectsInvalidHash(t *testing.T) {
	web := &WebConfig{PasswordHash: "not-a-bcrypt-hash"}
	_, _, err := EnsureWebPassword(web)
	require.Error(t, err)
}

func TestLoadOrCreateWebSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, WebSecretFileName)

	first, err := LoadOrCreateWebSecret(path)
	require.NoError(t, err)
	require.Len(t, first, 32)

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(first), strings.TrimSpace(string(raw)))

	second, err := LoadOrCreateWebSecret(path)
	require.NoError(t, err)
	require.Equal(t, first, second)
}

func TestLoadOrCreateWebSecretRejectsCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, WebSecretFileName)

	require.NoError(t, os.WriteFile(path, []byte("not-hex\n"), 0600))
	_, err := LoadOrCreateWebSecret(path)
	require.Error(t, err)

	require.NoError(t, os.WriteFile(path, []byte("abcd\n"), 0600))
	_, err = LoadOrCreateWebSecret(path)
	require.Error(t, err)
}

func TestServerConfigRecordsPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
listen: ":443"
auth:
  type: password
  password: "pw"
tls:
  cert: "cert.pem"
  key: "key.pem"
control:
  token: "0123456789abcdef0123456789abcdef"
`), 0600))

	cfg, err := LoadServer(path)
	require.NoError(t, err)
	require.Equal(t, path, cfg.ConfigPath)

	other := filepath.Join(dir, "other.yaml")
	require.NoError(t, SaveServer(other, cfg))
	require.Equal(t, other, cfg.ConfigPath)
}

func TestServerCredentialRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yaml")
	cfg := validServerConfig()
	cfg.Credentials = CredentialsConfig{
		Generation: 2, PreviousGeneration: 1,
		PreviousAuth: "old-auth", PreviousControlToken: "old-token",
	}
	require.NoError(t, SaveServer(path, cfg))
	got, err := LoadServer(path)
	require.NoError(t, err)
	require.Equal(t, int64(2), got.Credentials.Generation)
	require.Equal(t, int64(1), got.Credentials.PreviousGeneration)
	require.Equal(t, "old-auth", got.Credentials.PreviousAuth)
	require.Equal(t, "old-token", got.Credentials.PreviousControlToken)
}

func TestClientCredentialRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client.yaml")
	cfg := validClientConfig()
	cfg.Credentials = CredentialsConfig{
		Generation: 3, PreviousGeneration: 2,
		PreviousAuth: "old-auth", PreviousControlToken: "old-token",
	}
	require.NoError(t, SaveClient(path, cfg))
	got, err := LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, int64(3), got.Credentials.Generation)
	require.Equal(t, int64(2), got.Credentials.PreviousGeneration)
	require.Equal(t, "old-auth", got.Credentials.PreviousAuth)
	require.Equal(t, "old-token", got.Credentials.PreviousControlToken)
}

func TestServerConfigRejectsInvalidCredentialGenerations(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ServerConfig)
		wantErr string
	}{
		{
			name: "negative previous generation",
			mutate: func(cfg *ServerConfig) {
				cfg.Credentials.PreviousGeneration = -1
			},
			wantErr: "credentials.previous_generation",
		},
		{
			name: "previous generation not below current",
			mutate: func(cfg *ServerConfig) {
				cfg.Credentials.Generation = 2
				cfg.Credentials.PreviousGeneration = 2
				cfg.Credentials.PreviousAuth = "old-auth"
				cfg.Credentials.PreviousControlToken = "old-token"
			},
			wantErr: "credentials.previous_generation",
		},
		{
			name: "previous generation above current",
			mutate: func(cfg *ServerConfig) {
				cfg.Credentials.Generation = 2
				cfg.Credentials.PreviousGeneration = 3
				cfg.Credentials.PreviousAuth = "old-auth"
				cfg.Credentials.PreviousControlToken = "old-token"
			},
			wantErr: "credentials.previous_generation",
		},
		{
			name: "missing previous auth",
			mutate: func(cfg *ServerConfig) {
				cfg.Credentials.Generation = 2
				cfg.Credentials.PreviousGeneration = 1
				cfg.Credentials.PreviousControlToken = "old-token"
			},
			wantErr: "credentials.previous_auth",
		},
		{
			name: "missing previous control token",
			mutate: func(cfg *ServerConfig) {
				cfg.Credentials.Generation = 2
				cfg.Credentials.PreviousGeneration = 1
				cfg.Credentials.PreviousAuth = "old-auth"
			},
			wantErr: "credentials.previous_control_token",
		},
		{
			name: "previous auth without generation",
			mutate: func(cfg *ServerConfig) {
				cfg.Credentials.PreviousAuth = "old-auth"
			},
			wantErr: "credentials.previous_auth",
		},
		{
			name: "previous control token without generation",
			mutate: func(cfg *ServerConfig) {
				cfg.Credentials.PreviousControlToken = "old-token"
			},
			wantErr: "credentials.previous_control_token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validServerConfig()
			tt.mutate(cfg)
			require.ErrorContains(t, cfg.Validate(), tt.wantErr)
		})
	}
}

func TestServerConfigAllowsShortPreviousCredentials(t *testing.T) {
	cfg := validServerConfig()
	cfg.Credentials = CredentialsConfig{
		Generation: 2, PreviousGeneration: 1,
		PreviousAuth: "x", PreviousControlToken: "y",
	}
	require.NoError(t, cfg.Validate())
}

func TestClientConfigCredentialsGeneration(t *testing.T) {
	cfg := validClientConfig()
	cfg.Credentials.Generation = 0
	require.NoError(t, cfg.Validate())
	require.Equal(t, int64(1), cfg.Credentials.Generation)

	cfg = validClientConfig()
	cfg.Credentials.Generation = -1
	require.ErrorContains(t, cfg.Validate(), "credentials.generation")
}

func TestPreviousCredentialSecretsAreNotSerializedToJSON(t *testing.T) {
	cfg := validServerConfig()
	cfg.Credentials = CredentialsConfig{
		Generation: 2, PreviousGeneration: 1,
		PreviousAuth: "old-auth-secret", PreviousControlToken: "old-token-secret",
	}

	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.NotContains(t, string(data), "old-auth-secret")
	require.NotContains(t, string(data), "old-token-secret")
	require.Contains(t, string(data), `"generation":2`)
	require.Contains(t, string(data), `"previous_generation":1`)
}
