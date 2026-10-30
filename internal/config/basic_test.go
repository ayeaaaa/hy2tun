package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

const basicTestValidYAML = `server: "host:443"
auth: "secret-auth"
control:
  token: "0123456789abcdef"
client:
  name: "router"
`

func writeTestClientConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	return path
}

func TestGetBasicMissingFileReturnsDefaults(t *testing.T) {
	got, err := GetBasic(filepath.Join(t.TempDir(), "client.yaml"))
	require.NoError(t, err)
	require.False(t, got.Exists)
	require.False(t, got.Valid)
	require.Empty(t, got.ValidationError)
	require.Equal(t, "openwrt", got.ClientName)
	require.Equal(t, "0.0.0.0:7501", got.WebListen)
	require.Equal(t, "admin", got.WebUsername)
	require.Empty(t, got.Revision)
	require.Empty(t, got.Mappings)
}

func TestGetBasicReturnsRawValuesAndSecretsAsBooleans(t *testing.T) {
	path := writeTestClientConfig(t, `server: " Host:443 "
auth: "secret-auth"
control:
  token: "0123456789abcdef"
client:
  name: "router"
mappings:
  - id: ""
    type: tcp
    remote_port: 0
    local_addr: "127.0.0.1:22"
web:
  listen: "0.0.0.0:7501"
  username: ""
  password_hash: ""
`)
	got, err := GetBasic(path)
	require.NoError(t, err)
	require.Equal(t, " Host:443 ", got.Server)
	require.Equal(t, "host:443", got.ServerIdentity)
	require.True(t, got.AuthSet)
	require.True(t, got.ControlTokenSet)
	require.Empty(t, got.Mappings[0].ID)
	require.Equal(t, "", got.WebUsername)
	require.False(t, got.PasswordSet)
	require.False(t, got.WebTLS)
	require.False(t, got.Advanced)
	require.False(t, got.UnknownFields)
	// The empty mapping ID is returned as-is; Validate must not synthesize it
	// into the read model.
	require.Len(t, got.Mappings, 1)
	require.Equal(t, 0, got.Mappings[0].RemotePort)
}

func TestGetBasicParseFailureReturnsBasicError(t *testing.T) {
	path := writeTestClientConfig(t, "server: [unterminated\n")
	got, err := GetBasic(path)
	require.Nil(t, got)
	var basicErr *BasicError
	require.ErrorAs(t, err, &basicErr)
	require.Equal(t, BasicCodeParse, basicErr.Code)
	require.NotEmpty(t, basicErr.Message)
}

func TestGetBasicInvalidConfigReturnsFieldsAndValidationError(t *testing.T) {
	path := writeTestClientConfig(t, `server: "host:443"
control:
  token: "0123456789abcdef"
client:
  name: "router"
`)
	got, err := GetBasic(path)
	require.NoError(t, err)
	require.True(t, got.Exists)
	require.Equal(t, "host:443", got.Server)
	require.False(t, got.AuthSet)
	require.False(t, got.Valid)
	require.Contains(t, got.ValidationError, "auth is required")
	require.False(t, got.AdvancedError)
	require.False(t, got.Advanced)
}

func TestGetBasicDoesNotApplyValidateDefaults(t *testing.T) {
	path := writeTestClientConfig(t, `server: "host:443"
auth: "secret-auth"
control:
  token: "0123456789abcdef"
web:
  listen: "127.0.0.1:7501"
`)
	got, err := GetBasic(path)
	require.NoError(t, err)
	require.Empty(t, got.ClientName)
	require.Empty(t, got.WebUsername)
	require.Empty(t, got.LogLevel)
	require.Empty(t, got.Mappings)
	// Credential generation is the one normalized value.
	require.Equal(t, int64(1), got.CredentialGeneration)
}

func TestGetBasicMultiServerReturnsReadOnlyFields(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "client.log")
	path := writeTestClientConfig(t, fmt.Sprintf(`servers:
  - name: us
    server: "us.example.com:443"
    auth: "secret-us"
    control:
      token: "0123456789abcdef"
  - name: sg
    server: "sg.example.com:443"
    auth: "secret-sg"
    control:
      token: "fedcba9876543210"
web:
  listen: "127.0.0.1:7501"
log:
  file: %q
`, logFile))
	got, err := GetBasic(path)
	require.NoError(t, err)
	require.True(t, got.Exists)
	require.True(t, got.Multi)
	require.NotEmpty(t, got.Revision)
	require.True(t, got.Valid)
	require.Empty(t, got.ValidationError)
	require.Equal(t, "127.0.0.1:7501", got.WebListen)
	require.False(t, got.WebTLS)
	require.False(t, got.PasswordSet)
	require.Equal(t, logFile, got.LogFile)
	// Multi-server configs are read-only: editable and secret-bearing fields
	// stay empty.
	require.Empty(t, got.Server)
	require.Empty(t, got.ServerIdentity)
	require.Empty(t, got.ClientName)
	require.Empty(t, got.WebUsername)
	require.Empty(t, got.Mappings)
	require.False(t, got.AuthSet)
	require.False(t, got.ControlTokenSet)
	require.False(t, got.ServerLocked)
	require.Equal(t, int64(0), got.CredentialGeneration)
}

func TestGetBasicMultiServerWithoutPasswordIsValid(t *testing.T) {
	path := writeTestClientConfig(t, `servers:
  - name: us
    server: "us.example.com:443"
    auth: "secret-us"
    control:
      token: "0123456789abcdef"
web:
  listen: "127.0.0.1:7501"
`)
	got, err := GetBasic(path)
	require.NoError(t, err)
	require.True(t, got.Multi)
	require.True(t, got.Valid)
	require.Empty(t, got.ValidationError)
}

func TestGetBasicSingleServerWebPasswordPrecondition(t *testing.T) {
	path := writeTestClientConfig(t, basicTestValidYAML+`web:
  listen: "127.0.0.1:7501"
`)
	got, err := GetBasic(path)
	require.NoError(t, err)
	require.False(t, got.Valid)
	require.NotEmpty(t, got.ValidationError)
	require.False(t, got.AdvancedError)

	hash, err := bcrypt.GenerateFromPassword([]byte("password123"), bcrypt.MinCost)
	require.NoError(t, err)
	path = writeTestClientConfig(t, basicTestValidYAML+fmt.Sprintf(`web:
  listen: "127.0.0.1:7501"
  username: "admin"
  password_hash: %q
`, string(hash)))
	got, err = GetBasic(path)
	require.NoError(t, err)
	require.True(t, got.Valid)
	require.Empty(t, got.ValidationError)
	require.True(t, got.PasswordSet)
}

func TestGetBasicAdvancedFields(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "client.log")
	cases := map[string]string{
		"tls ca": `tls:
  ca: "/definitely/missing/ca.pem"
`,
		"tls pin": `tls:
  pin_sha256: "abcdef"
`,
		"obfs": `obfs:
  type: salamander
  salamander:
    password: "obfs-secret"
`,
		"bandwidth": `bandwidth:
  up: "10 mbps"
`,
		"transport": `transport:
  udp: {}
`,
		"proxy": `proxy:
  socks5: "127.0.0.1:1080"
`,
		"web tls": `web:
  tls:
    cert: "/definitely/missing/cert.pem"
    key: "/definitely/missing/key.pem"
`,
		"web trust proxy": `web:
  trust_proxy: true
`,
		"log debug": `log:
  level: "debug"
`,
		"log file": fmt.Sprintf("log:\n  file: %q\n", logFile),
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTestClientConfig(t, basicTestValidYAML+extra)
			got, err := GetBasic(path)
			require.NoError(t, err)
			require.True(t, got.Advanced)
		})
	}
}

func TestGetBasicPlainConfigIsNotAdvanced(t *testing.T) {
	path := writeTestClientConfig(t, basicTestValidYAML+`web:
  listen: ""
log:
  level: "info"
`)
	got, err := GetBasic(path)
	require.NoError(t, err)
	require.False(t, got.Advanced)
	require.True(t, got.Valid)
}

func TestGetBasicUnknownTopLevelKeys(t *testing.T) {
	path := writeTestClientConfig(t, basicTestValidYAML+"mystery: true\n")
	got, err := GetBasic(path)
	require.NoError(t, err)
	require.True(t, got.UnknownFields)

	known := writeTestClientConfig(t, `server: "host:443"
servers: []
auth: "secret-auth"
tls:
  sni: "host"
  insecure: false
obfs: {}
bandwidth: {}
transport: {}
control:
  token: "0123456789abcdef"
client:
  name: "router"
mappings: []
web:
  listen: ""
proxy: {}
log:
  level: "info"
credentials:
  generation: 1
`)
	got, err = GetBasic(known)
	require.NoError(t, err)
	require.False(t, got.UnknownFields)
}

func TestGetBasicCheckClientFailureIsAdvancedError(t *testing.T) {
	cases := map[string]string{
		"missing ca": `tls:
  ca: "/definitely/missing/ca.pem"
`,
		"invalid bcrypt": `web:
  listen: "127.0.0.1:7501"
  username: "admin"
  password_hash: "not-a-bcrypt-hash"
`,
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTestClientConfig(t, basicTestValidYAML+extra)
			got, err := GetBasic(path)
			require.NoError(t, err)
			require.False(t, got.Valid)
			require.True(t, got.AdvancedError)
			require.NotEmpty(t, got.ValidationError)
		})
	}
}

func TestGetBasicServerLocked(t *testing.T) {
	cases := map[string]struct {
		extra string
		want  bool
	}{
		"plain":     {extra: "", want: false},
		"tls ca":    {extra: "tls:\n  ca: \"/tmp/ca.pem\"\n", want: true},
		"tls pin":   {extra: "tls:\n  pin_sha256: \"abcdef\"\n", want: true},
		"obfs":      {extra: "obfs:\n  type: salamander\n", want: true},
		"bandwidth": {extra: "bandwidth:\n  up: \"1 mbps\"\n", want: true},
		"transport": {extra: "transport: {}\n", want: true},
		"generation": {
			extra: "credentials:\n  generation: 2\n",
			want:  true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTestClientConfig(t, basicTestValidYAML+tc.extra)
			got, err := GetBasic(path)
			require.NoError(t, err)
			require.Equal(t, tc.want, got.ServerLocked)
		})
	}
}

func TestGetBasicNormalizesCredentialGeneration(t *testing.T) {
	path := writeTestClientConfig(t, basicTestValidYAML)
	got, err := GetBasic(path)
	require.NoError(t, err)
	require.Equal(t, int64(1), got.CredentialGeneration)

	path = writeTestClientConfig(t, basicTestValidYAML+"credentials:\n  generation: 3\n")
	got, err = GetBasic(path)
	require.NoError(t, err)
	require.Equal(t, int64(3), got.CredentialGeneration)
	require.True(t, got.ServerLocked)
	// Credential generation is runtime metadata, not an advanced field.
	require.False(t, got.Advanced)
}

func TestGetBasicInvalidServerLeavesIdentityEmpty(t *testing.T) {
	path := writeTestClientConfig(t, `server: "missing-port"
auth: "secret-auth"
control:
  token: "0123456789abcdef"
client:
  name: "router"
`)
	got, err := GetBasic(path)
	require.NoError(t, err)
	require.Empty(t, got.ServerIdentity)
	require.False(t, got.Valid)
	require.Contains(t, got.ValidationError, "host:port")
	require.False(t, got.AdvancedError)
}

func TestGetBasicFileRevision(t *testing.T) {
	revision, err := FileRevision(filepath.Join(t.TempDir(), "missing.yaml"))
	require.NoError(t, err)
	require.Empty(t, revision)

	body := "server: \"host:443\"\n"
	path := writeTestClientConfig(t, body)
	revision, err = FileRevision(path)
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(body))
	require.Equal(t, hex.EncodeToString(sum[:]), revision)
	require.Len(t, revision, 64)

	got, err := GetBasic(path)
	require.NoError(t, err)
	require.Equal(t, revision, got.Revision)
}
