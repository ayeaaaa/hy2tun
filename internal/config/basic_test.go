package config

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"hy2tun/internal/testutil"

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

func basicStringPtr(value string) *string { return &value }

func basicBoolPtr(value bool) *bool { return &value }

func basicMappingsPtr(value []Mapping) *[]Mapping { return &value }

// basicUpdate builds the required fields of an update. Secrets are left nil so
// callers can opt into preserving or replacing them.
func basicUpdate(server, name, listen, username string, mappings []Mapping) *BasicUpdate {
	return &BasicUpdate{
		Server:      basicStringPtr(server),
		ClientName:  basicStringPtr(name),
		Mappings:    basicMappingsPtr(mappings),
		WebListen:   basicStringPtr(listen),
		WebUsername: basicStringPtr(username),
	}
}

func requireBasicErrorCode(t *testing.T, err error, code BasicErrorCode) *BasicError {
	t.Helper()
	var basicErr *BasicError
	require.ErrorAs(t, err, &basicErr)
	require.Equal(t, code, basicErr.Code, basicErr.Message)
	return basicErr
}

func readBasicTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

// writeBasicTestCert writes a self-signed certificate and key as PEM files.
func writeBasicTestCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0600))
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0600))
	return certPath, keyPath
}

func TestSetBasicCreatesConfigAndHashesPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	server := "host:443"
	auth := "auth-secret"
	token := "0123456789abcdef"
	name := "router"
	mappings := []Mapping{{Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:22", Enabled: true}}
	listen := "0.0.0.0:7501"
	username := "admin"
	password := "password123"
	confirm := "password123"
	got, err := SetBasic(path, &BasicUpdate{
		Server: &server, Auth: &auth, ControlToken: &token,
		ClientName: &name, Mappings: &mappings,
		WebListen: &listen, WebUsername: &username,
		WebPassword: &password, WebPasswordConfirm: &confirm,
	}, SetBasicOptions{})
	require.NoError(t, err)
	require.True(t, got.Exists)
	require.True(t, got.Valid)
	require.NotEmpty(t, got.Revision)
	require.Len(t, got.Mappings, 1)
	require.NotEmpty(t, got.Mappings[0].ID)
	require.True(t, got.AuthSet)
	require.True(t, got.ControlTokenSet)
	require.True(t, got.PasswordSet)
	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(cfg.Web.PasswordHash), []byte(password)))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	lockInfo, err := os.Stat(path + ".lock")
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), lockInfo.Mode().Perm())
}

func TestSetBasicBlankSecretsPreserveExistingValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	password := "password123"
	confirm := "password123"
	initial, err := SetBasic(path, &BasicUpdate{
		Server:             basicStringPtr("host:443"),
		Auth:               basicStringPtr("existing-auth"),
		ControlToken:       basicStringPtr("0123456789abcdef"),
		ClientName:         basicStringPtr("router"),
		Mappings:           basicMappingsPtr([]Mapping{}),
		WebListen:          basicStringPtr("127.0.0.1:7501"),
		WebUsername:        basicStringPtr("admin"),
		WebPassword:        &password,
		WebPasswordConfirm: &confirm,
	}, SetBasicOptions{})
	require.NoError(t, err)
	before, err := LoadClient(path)
	require.NoError(t, err)
	hash := before.Web.PasswordHash

	blank := ""
	got, err := SetBasic(path, &BasicUpdate{
		ExpectedRevision: initial.Revision,
		Server:           basicStringPtr("host:443"),
		Auth:             &blank,
		ControlToken:     &blank,
		ClientName:       basicStringPtr("router-renamed"),
		Mappings:         basicMappingsPtr(nil),
		WebListen:        basicStringPtr("127.0.0.1:7501"),
		WebUsername:      basicStringPtr("admin"),
		WebPassword:      &blank,
	}, SetBasicOptions{})
	require.NoError(t, err)
	require.True(t, got.AuthSet)
	require.True(t, got.ControlTokenSet)
	require.True(t, got.PasswordSet)

	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, "existing-auth", cfg.Auth)
	require.Equal(t, "0123456789abcdef", cfg.Control.Token)
	require.Equal(t, "router-renamed", cfg.Client.Name)
	require.Equal(t, hash, cfg.Web.PasswordHash)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(cfg.Web.PasswordHash), []byte(password)))
}

func TestSetBasicNewConfigRequiresSecrets(t *testing.T) {
	base := func() *BasicUpdate {
		return &BasicUpdate{
			Server:      basicStringPtr("host:443"),
			ClientName:  basicStringPtr("router"),
			Mappings:    basicMappingsPtr([]Mapping{}),
			WebListen:   basicStringPtr(""),
			WebUsername: basicStringPtr(""),
		}
	}
	cases := map[string]func(*BasicUpdate){
		"auth": func(u *BasicUpdate) {
			u.ControlToken = basicStringPtr("0123456789abcdef")
		},
		"token": func(u *BasicUpdate) {
			u.Auth = basicStringPtr("auth-secret")
		},
		"password": func(u *BasicUpdate) {
			u.Auth = basicStringPtr("auth-secret")
			u.ControlToken = basicStringPtr("0123456789abcdef")
			u.WebListen = basicStringPtr("127.0.0.1:7501")
			u.WebUsername = basicStringPtr("admin")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "client.yaml")
			update := base()
			mutate(update)
			_, err := SetBasic(path, update, SetBasicOptions{})
			requireBasicErrorCode(t, err, BasicCodeValidation)
			_, statErr := os.Stat(path)
			require.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}
}

func TestSetBasicPasswordRules(t *testing.T) {
	newUpdate := func(password string, confirm *string) *BasicUpdate {
		return &BasicUpdate{
			Server:             basicStringPtr("host:443"),
			Auth:               basicStringPtr("auth-secret"),
			ControlToken:       basicStringPtr("0123456789abcdef"),
			ClientName:         basicStringPtr("router"),
			Mappings:           basicMappingsPtr([]Mapping{}),
			WebListen:          basicStringPtr("127.0.0.1:7501"),
			WebUsername:        basicStringPtr("admin"),
			WebPassword:        basicStringPtr(password),
			WebPasswordConfirm: confirm,
		}
	}

	cases := map[string]*BasicUpdate{
		"too short":  newUpdate("1234567", basicStringPtr("1234567")),
		"mismatch":   newUpdate("password123", basicStringPtr("password124")),
		"no confirm": newUpdate("password123", nil),
		"disabled listen": func() *BasicUpdate {
			u := newUpdate("password123", basicStringPtr("password123"))
			u.WebListen = basicStringPtr("")
			return u
		}(),
	}
	for name, update := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "client.yaml")
			_, err := SetBasic(path, update, SetBasicOptions{})
			requireBasicErrorCode(t, err, BasicCodeValidation)
			_, statErr := os.Stat(path)
			require.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}

	t.Run("minimum length accepted", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "client.yaml")
		password := "12345678"
		_, err := SetBasic(path, newUpdate(password, basicStringPtr(password)), SetBasicOptions{})
		require.NoError(t, err)
		cfg, err := LoadClient(path)
		require.NoError(t, err)
		require.NoError(t, bcrypt.CompareHashAndPassword([]byte(cfg.Web.PasswordHash), []byte(password)))
	})
}

func TestSetBasicDisablingWebClearsPasswordAndReenableRequiresNewOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	password := "password123"
	confirm := "password123"
	initial, err := SetBasic(path, &BasicUpdate{
		Server:             basicStringPtr("host:443"),
		Auth:               basicStringPtr("auth-secret"),
		ControlToken:       basicStringPtr("0123456789abcdef"),
		ClientName:         basicStringPtr("router"),
		Mappings:           basicMappingsPtr([]Mapping{}),
		WebListen:          basicStringPtr("127.0.0.1:7501"),
		WebUsername:        basicStringPtr("admin"),
		WebPassword:        &password,
		WebPasswordConfirm: &confirm,
	}, SetBasicOptions{})
	require.NoError(t, err)

	disabled, err := SetBasic(path, &BasicUpdate{
		ExpectedRevision: initial.Revision,
		Server:           basicStringPtr("host:443"),
		ClientName:       basicStringPtr("router"),
		Mappings:         basicMappingsPtr([]Mapping{}),
		WebListen:        basicStringPtr(""),
		WebUsername:      basicStringPtr("admin"),
	}, SetBasicOptions{})
	require.NoError(t, err)
	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.Empty(t, cfg.Web.Listen)
	require.Empty(t, cfg.Web.PasswordHash)

	_, err = SetBasic(path, &BasicUpdate{
		ExpectedRevision: disabled.Revision,
		Server:           basicStringPtr("host:443"),
		ClientName:       basicStringPtr("router"),
		Mappings:         basicMappingsPtr([]Mapping{}),
		WebListen:        basicStringPtr("127.0.0.1:7501"),
		WebUsername:      basicStringPtr("admin"),
	}, SetBasicOptions{})
	requireBasicErrorCode(t, err, BasicCodeValidation)

	newPassword := "newpassword"
	_, err = SetBasic(path, &BasicUpdate{
		ExpectedRevision:   disabled.Revision,
		Server:             basicStringPtr("host:443"),
		ClientName:         basicStringPtr("router"),
		Mappings:           basicMappingsPtr([]Mapping{}),
		WebListen:          basicStringPtr("127.0.0.1:7501"),
		WebUsername:        basicStringPtr("admin"),
		WebPassword:        &newPassword,
		WebPasswordConfirm: &newPassword,
	}, SetBasicOptions{})
	require.NoError(t, err)
	cfg, err = LoadClient(path)
	require.NoError(t, err)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(cfg.Web.PasswordHash), []byte(newPassword)))
}

func TestSetBasicRevisionConflicts(t *testing.T) {
	path := writeTestClientConfig(t, basicTestValidYAML)
	original := readBasicTestFile(t, path)

	update := basicUpdate("host:443", "router", "", "", []Mapping{})
	update.ExpectedRevision = ""
	_, err := SetBasic(path, update, SetBasicOptions{})
	requireBasicErrorCode(t, err, BasicCodeConflict)

	update.ExpectedRevision = fmt.Sprintf("%064d", 0)
	_, err = SetBasic(path, update, SetBasicOptions{})
	requireBasicErrorCode(t, err, BasicCodeConflict)
	require.Equal(t, original, readBasicTestFile(t, path))

	newPath := filepath.Join(t.TempDir(), "client.yaml")
	newUpdate := basicUpdate("host:443", "router", "", "", []Mapping{})
	newUpdate.Auth = basicStringPtr("auth-secret")
	newUpdate.ControlToken = basicStringPtr("0123456789abcdef")
	newUpdate.ExpectedRevision = "stale"
	_, err = SetBasic(newPath, newUpdate, SetBasicOptions{})
	requireBasicErrorCode(t, err, BasicCodeConflict)
	_, statErr := os.Stat(newPath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestSetBasicBeforeWriteCanBlockSave(t *testing.T) {
	path := writeTestClientConfig(t, basicTestValidYAML)
	original := readBasicTestFile(t, path)
	revision, err := FileRevision(path)
	require.NoError(t, err)
	update := basicUpdate("host:443", "changed", "", "", []Mapping{})
	update.ExpectedRevision = revision

	called := false
	_, err = SetBasic(path, update, SetBasicOptions{BeforeWrite: func() error {
		called = true
		return &BasicError{Code: BasicCodeServiceRunning, Message: "service is running"}
	}})
	require.True(t, called)
	requireBasicErrorCode(t, err, BasicCodeServiceRunning)
	require.Equal(t, original, readBasicTestFile(t, path))

	_, err = SetBasic(path, update, SetBasicOptions{BeforeWrite: func() error {
		return errors.New("boom")
	}})
	requireBasicErrorCode(t, err, BasicCodeInternal)
	require.Equal(t, original, readBasicTestFile(t, path))

	called = false
	conflict := *update
	conflict.ExpectedRevision = fmt.Sprintf("%064d", 0)
	_, err = SetBasic(path, &conflict, SetBasicOptions{BeforeWrite: func() error {
		called = true
		return nil
	}})
	requireBasicErrorCode(t, err, BasicCodeConflict)
	require.False(t, called)
}

func TestSetBasicConcurrentWritersConflict(t *testing.T) {
	path := writeTestClientConfig(t, basicTestValidYAML)
	revision, err := FileRevision(path)
	require.NoError(t, err)

	type result struct {
		name string
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, name := range []string{"one", "two"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			<-start
			update := basicUpdate("host:443", name, "", "", []Mapping{})
			update.ExpectedRevision = revision
			_, err := SetBasic(path, update, SetBasicOptions{})
			results <- result{name: name, err: err}
		}(name)
	}
	close(start)
	wg.Wait()
	close(results)

	successes := 0
	conflicts := 0
	successName := ""
	for res := range results {
		if res.err == nil {
			successes++
			successName = res.name
			continue
		}
		requireBasicErrorCode(t, res.err, BasicCodeConflict)
		conflicts++
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)
	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, successName, cfg.Client.Name)
}

func TestSetBasicRejectsMultiServerAndUnknownFields(t *testing.T) {
	multi := `servers:
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
`
	path := writeTestClientConfig(t, multi)
	original := readBasicTestFile(t, path)
	revision, err := FileRevision(path)
	require.NoError(t, err)
	update := basicUpdate("us.example.com:443", "router", "", "", []Mapping{})
	update.ExpectedRevision = revision
	_, err = SetBasic(path, update, SetBasicOptions{})
	requireBasicErrorCode(t, err, BasicCodeMulti)
	require.Equal(t, original, readBasicTestFile(t, path))

	unknownPath := writeTestClientConfig(t, basicTestValidYAML+"mystery: true\n")
	unknownOriginal := readBasicTestFile(t, unknownPath)
	revision, err = FileRevision(unknownPath)
	require.NoError(t, err)
	update.ExpectedRevision = revision
	_, err = SetBasic(unknownPath, update, SetBasicOptions{})
	requireBasicErrorCode(t, err, BasicCodeUnknownFields)
	require.Equal(t, unknownOriginal, readBasicTestFile(t, unknownPath))
}

func TestSetBasicServerMigrationRotatesIdentity(t *testing.T) {
	path := writeTestClientConfig(t, `server: "old.example:443"
auth: "old-auth-secret"
control:
  token: "0123456789abcdef"
client:
  id: "c-old"
  name: "router"
  revision: 9
tls:
  sni: "old.example"
`)
	revision, err := FileRevision(path)
	require.NoError(t, err)
	update := basicUpdate("new.example:443", "router", "", "", []Mapping{})
	update.ExpectedRevision = revision
	update.Auth = basicStringPtr("new-auth-secret")
	update.ControlToken = basicStringPtr("fedcba9876543210")
	update.TLSSNI = basicStringPtr("old.example")
	got, err := SetBasic(path, update, SetBasicOptions{})
	require.NoError(t, err)
	require.Equal(t, "new.example:443", got.Server)
	require.Empty(t, got.TLSSNI)

	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, "new.example:443", cfg.Server)
	require.Equal(t, "new-auth-secret", cfg.Auth)
	require.Equal(t, "fedcba9876543210", cfg.Control.Token)
	require.Empty(t, cfg.Client.ID)
	require.Zero(t, cfg.Client.Revision)
	require.Empty(t, cfg.TLS.SNI)
	require.Equal(t, int64(1), cfg.Credentials.Generation)
}

func TestSetBasicServerMigrationKeepsCustomSNI(t *testing.T) {
	path := writeTestClientConfig(t, `server: "old.example:443"
auth: "old-auth-secret"
control:
  token: "0123456789abcdef"
client:
  id: "c-old"
  name: "router"
  revision: 9
tls:
  sni: "old.example"
`)
	revision, err := FileRevision(path)
	require.NoError(t, err)
	update := basicUpdate("new.example:443", "router", "", "", []Mapping{})
	update.ExpectedRevision = revision
	update.Auth = basicStringPtr("new-auth-secret")
	update.ControlToken = basicStringPtr("fedcba9876543210")
	update.TLSSNI = basicStringPtr("new.example")
	_, err = SetBasic(path, update, SetBasicOptions{})
	require.NoError(t, err)
	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, "new.example", cfg.TLS.SNI)
}

func TestSetBasicServerMigrationRequiresNewCredentials(t *testing.T) {
	cases := map[string]func(*BasicUpdate){
		"missing auth": func(u *BasicUpdate) {
			u.ControlToken = basicStringPtr("fedcba9876543210")
		},
		"blank auth": func(u *BasicUpdate) {
			u.Auth = basicStringPtr("")
			u.ControlToken = basicStringPtr("fedcba9876543210")
		},
		"missing token": func(u *BasicUpdate) {
			u.Auth = basicStringPtr("new-auth-secret")
		},
		"blank token": func(u *BasicUpdate) {
			u.Auth = basicStringPtr("new-auth-secret")
			u.ControlToken = basicStringPtr("")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTestClientConfig(t, basicTestValidYAML)
			original := readBasicTestFile(t, path)
			revision, err := FileRevision(path)
			require.NoError(t, err)
			update := basicUpdate("new.example:443", "router", "", "", []Mapping{})
			update.ExpectedRevision = revision
			mutate(update)
			_, err = SetBasic(path, update, SetBasicOptions{})
			requireBasicErrorCode(t, err, BasicCodeValidation)
			require.Equal(t, original, readBasicTestFile(t, path))
		})
	}
}

func TestSetBasicEquivalentServerSpellingDoesNotMigrate(t *testing.T) {
	path := writeTestClientConfig(t, `server: "host:443"
auth: "old-auth-secret"
control:
  token: "0123456789abcdef"
client:
  id: "c-keep"
  name: "router"
  revision: 4
tls:
  sni: "sni.example"
`)
	revision, err := FileRevision(path)
	require.NoError(t, err)
	update := basicUpdate("HOST:443", "router", "", "", []Mapping{})
	update.ExpectedRevision = revision
	update.TLSSNI = basicStringPtr("sni.example")
	_, err = SetBasic(path, update, SetBasicOptions{})
	require.NoError(t, err)

	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, "HOST:443", cfg.Server)
	require.Equal(t, "old-auth-secret", cfg.Auth)
	require.Equal(t, "0123456789abcdef", cfg.Control.Token)
	require.Equal(t, "c-keep", cfg.Client.ID)
	require.Equal(t, int64(4), cfg.Client.Revision)
	require.Equal(t, "sni.example", cfg.TLS.SNI)
}

func TestSetBasicServerMigrationRejectsAdvancedConfigs(t *testing.T) {
	cases := map[string]string{
		"tls ca":     "tls:\n  ca: \"/nonexistent/ca.pem\"\n",
		"tls pin":    "tls:\n  pin_sha256: \"abcdef\"\n",
		"obfs":       "obfs:\n  type: salamander\n",
		"bandwidth":  "bandwidth:\n  up: \"1 mbps\"\n",
		"transport":  "transport:\n  udp: {}\n",
		"generation": "credentials:\n  generation: 2\n",
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeTestClientConfig(t, basicTestValidYAML+extra)
			original := readBasicTestFile(t, path)
			revision, err := FileRevision(path)
			require.NoError(t, err)
			update := basicUpdate("new.example:443", "router", "", "", []Mapping{})
			update.ExpectedRevision = revision
			update.Auth = basicStringPtr("new-auth-secret")
			update.ControlToken = basicStringPtr("fedcba9876543210")
			_, err = SetBasic(path, update, SetBasicOptions{})
			basicErr := requireBasicErrorCode(t, err, BasicCodeAdvanced)
			require.True(t, basicErr.Advanced)
			require.Equal(t, original, readBasicTestFile(t, path))
		})
	}
}

func TestSetBasicRejectsMissingRequiredFields(t *testing.T) {
	cases := map[string]func(*BasicUpdate){
		"server":       func(u *BasicUpdate) { u.Server = nil },
		"client name":  func(u *BasicUpdate) { u.ClientName = nil },
		"mappings":     func(u *BasicUpdate) { u.Mappings = nil },
		"web listen":   func(u *BasicUpdate) { u.WebListen = nil },
		"web username": func(u *BasicUpdate) { u.WebUsername = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "client.yaml")
			update := basicUpdate("host:443", "router", "", "", []Mapping{})
			mutate(update)
			_, err := SetBasic(path, update, SetBasicOptions{})
			requireBasicErrorCode(t, err, BasicCodeValidation)
			_, statErr := os.Stat(path)
			require.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}

	t.Run("nil update", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "client.yaml")
		_, err := SetBasic(path, nil, SetBasicOptions{})
		requireBasicErrorCode(t, err, BasicCodeValidation)
	})
}

func TestSetBasicAcceptsExplicitEmptyMappingsAndWebListen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	update := &BasicUpdate{
		Server:       basicStringPtr("host:443"),
		Auth:         basicStringPtr("auth-secret"),
		ControlToken: basicStringPtr("0123456789abcdef"),
		ClientName:   basicStringPtr("router"),
		Mappings:     basicMappingsPtr([]Mapping{}),
		WebListen:    basicStringPtr(""),
		WebUsername:  basicStringPtr(""),
	}
	got, err := SetBasic(path, update, SetBasicOptions{})
	require.NoError(t, err)
	require.Empty(t, got.Mappings)
	require.Empty(t, got.WebListen)
	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.Empty(t, cfg.Mappings)
	require.Empty(t, cfg.Web.Listen)
	require.Empty(t, cfg.Web.PasswordHash)
}

func TestSetBasicPreservesAdvancedFields(t *testing.T) {
	certPath, keyPath := writeBasicTestCert(t)
	logPath := filepath.Join(t.TempDir(), "client.log")
	path := writeTestClientConfig(t, fmt.Sprintf(`server: "host:443"
auth: "secret-auth"
control:
  token: "0123456789abcdef"
client:
  name: "router"
tls:
  sni: "host"
  pin_sha256: "abcdef"
obfs:
  type: salamander
  salamander:
    password: "obfs-secret"
proxy:
  enabled: true
  socks5: "127.0.0.1:1080"
web:
  listen: ""
  tls:
    cert: %q
    key: %q
  trust_proxy: true
log:
  level: "debug"
  file: %q
`, certPath, keyPath, logPath))
	revision, err := FileRevision(path)
	require.NoError(t, err)
	update := basicUpdate("host:443", "router-renamed", "", "admin", []Mapping{})
	update.ExpectedRevision = revision
	got, err := SetBasic(path, update, SetBasicOptions{})
	require.NoError(t, err)
	require.True(t, got.Advanced)

	cfg, _, err := loadClientForEdit(path)
	require.NoError(t, err)
	require.Equal(t, "abcdef", cfg.TLS.PinSHA256)
	require.Equal(t, "host", cfg.TLS.SNI)
	require.Equal(t, "salamander", cfg.Obfs.Type)
	require.Equal(t, "obfs-secret", cfg.Obfs.Salamander.Password)
	require.True(t, cfg.Proxy.Enabled)
	require.Equal(t, "127.0.0.1:1080", cfg.Proxy.SOCKS5)
	require.Equal(t, certPath, cfg.Web.TLS.Cert)
	require.Equal(t, keyPath, cfg.Web.TLS.Key)
	require.True(t, cfg.Web.TrustProxy)
	require.Equal(t, "debug", cfg.Log.Level)
	require.Equal(t, logPath, cfg.Log.File)
	require.Equal(t, "router-renamed", cfg.Client.Name)
}

func TestSetBasicFailedValidationLeavesFilesUntouched(t *testing.T) {
	path := writeTestClientConfig(t, basicTestValidYAML)
	original := readBasicTestFile(t, path)
	backupPath := path + ".bak"
	backup := []byte("previous backup\n")
	require.NoError(t, os.WriteFile(backupPath, backup, 0600))

	revision, err := FileRevision(path)
	require.NoError(t, err)
	update := basicUpdate("host:443", "router", "127.0.0.1:7501", "admin", []Mapping{})
	update.ExpectedRevision = revision
	update.WebPassword = basicStringPtr("short")
	update.WebPasswordConfirm = basicStringPtr("short")
	_, err = SetBasic(path, update, SetBasicOptions{})
	requireBasicErrorCode(t, err, BasicCodeValidation)
	require.Equal(t, original, readBasicTestFile(t, path))
	require.Equal(t, backup, readBasicTestFile(t, backupPath))

	newPath := filepath.Join(t.TempDir(), "client.yaml")
	newUpdate := basicUpdate("host:443", "router", "", "", []Mapping{})
	newUpdate.Auth = basicStringPtr("auth-secret")
	_, err = SetBasic(newPath, newUpdate, SetBasicOptions{})
	requireBasicErrorCode(t, err, BasicCodeValidation)
	_, statErr := os.Stat(newPath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
	_, statErr = os.Stat(newPath + ".bak")
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestSetBasicSuccessfulOverwriteKeepsBackupAndMode(t *testing.T) {
	path := writeTestClientConfig(t, basicTestValidYAML)
	original := readBasicTestFile(t, path)
	revision, err := FileRevision(path)
	require.NoError(t, err)
	update := basicUpdate("host:443", "renamed", "", "", []Mapping{})
	update.ExpectedRevision = revision
	_, err = SetBasic(path, update, SetBasicOptions{})
	require.NoError(t, err)

	require.Equal(t, original, readBasicTestFile(t, path+".bak"))
	for _, name := range []string{path, path + ".bak"} {
		info, err := os.Stat(name)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}
}

func TestSetBasicGeneratedMappingIDsSurviveSecondSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.yaml")
	mappings := []Mapping{{Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:22", Enabled: true}}
	first, err := SetBasic(path, &BasicUpdate{
		Server:       basicStringPtr("host:443"),
		Auth:         basicStringPtr("auth-secret"),
		ControlToken: basicStringPtr("0123456789abcdef"),
		ClientName:   basicStringPtr("router"),
		Mappings:     &mappings,
		WebListen:    basicStringPtr(""),
		WebUsername:  basicStringPtr(""),
	}, SetBasicOptions{})
	require.NoError(t, err)
	require.Len(t, first.Mappings, 1)
	id := first.Mappings[0].ID
	require.NotEmpty(t, id)

	second, err := SetBasic(path, &BasicUpdate{
		ExpectedRevision: first.Revision,
		Server:           basicStringPtr("host:443"),
		ClientName:       basicStringPtr("router"),
		Mappings:         basicMappingsPtr(first.Mappings),
		WebListen:        basicStringPtr(""),
		WebUsername:      basicStringPtr(""),
	}, SetBasicOptions{})
	require.NoError(t, err)
	require.Len(t, second.Mappings, 1)
	require.Equal(t, id, second.Mappings[0].ID)

	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.Len(t, cfg.Mappings, 1)
	require.Equal(t, id, cfg.Mappings[0].ID)
}

func TestSetBasicAuthAndTokenRules(t *testing.T) {
	path := writeTestClientConfig(t, basicTestValidYAML)
	revision, err := FileRevision(path)
	require.NoError(t, err)
	base := func() *BasicUpdate {
		update := basicUpdate("host:443", "router", "", "", []Mapping{})
		update.ExpectedRevision = revision
		return update
	}

	update := base()
	update.Auth = basicStringPtr("short")
	_, err = SetBasic(path, update, SetBasicOptions{})
	requireBasicErrorCode(t, err, BasicCodeValidation)

	update = base()
	update.ControlToken = basicStringPtr("too-short")
	_, err = SetBasic(path, update, SetBasicOptions{})
	requireBasicErrorCode(t, err, BasicCodeValidation)

	update = base()
	update.Auth = basicStringPtr("12345678")
	got, err := SetBasic(path, update, SetBasicOptions{})
	require.NoError(t, err)
	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, "12345678", cfg.Auth)
	require.Equal(t, "0123456789abcdef", cfg.Control.Token)

	revision = got.Revision
	update = base()
	update.ExpectedRevision = revision
	update.ControlToken = basicStringPtr("fedcba9876543210")
	_, err = SetBasic(path, update, SetBasicOptions{})
	require.NoError(t, err)
	cfg, err = LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, "12345678", cfg.Auth)
	require.Equal(t, "fedcba9876543210", cfg.Control.Token)
}

func TestSetBasicCredentialGenerationLocksAuthChanges(t *testing.T) {
	path := writeTestClientConfig(t, basicTestValidYAML+"credentials:\n  generation: 2\n")
	revision, err := FileRevision(path)
	require.NoError(t, err)

	cases := map[string]func(*BasicUpdate){
		"auth": func(u *BasicUpdate) {
			u.Auth = basicStringPtr("new-auth-secret")
		},
		"token": func(u *BasicUpdate) {
			u.ControlToken = basicStringPtr("fedcba9876543210")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			update := basicUpdate("host:443", "router", "", "", []Mapping{})
			update.ExpectedRevision = revision
			mutate(update)
			_, err := SetBasic(path, update, SetBasicOptions{})
			basicErr := requireBasicErrorCode(t, err, BasicCodeAdvanced)
			require.True(t, basicErr.Advanced)
		})
	}

	update := basicUpdate("host:443", "renamed", "", "", []Mapping{})
	update.ExpectedRevision = revision
	_, err = SetBasic(path, update, SetBasicOptions{})
	require.NoError(t, err)
}

func TestSetBasicUpdatesTLSSNIAndInsecure(t *testing.T) {
	path := writeTestClientConfig(t, basicTestValidYAML)
	revision, err := FileRevision(path)
	require.NoError(t, err)
	update := basicUpdate("host:443", "router", "", "", []Mapping{})
	update.ExpectedRevision = revision
	update.TLSSNI = basicStringPtr("custom.example")
	update.TLSInsecure = basicBoolPtr(true)
	got, err := SetBasic(path, update, SetBasicOptions{})
	require.NoError(t, err)
	require.Equal(t, "custom.example", got.TLSSNI)
	require.True(t, got.TLSInsecure)

	cfg, err := LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, "custom.example", cfg.TLS.SNI)
	require.True(t, cfg.TLS.Insecure)

	revision = got.Revision
	update = basicUpdate("host:443", "router", "", "", []Mapping{})
	update.ExpectedRevision = revision
	update.TLSSNI = basicStringPtr("")
	_, err = SetBasic(path, update, SetBasicOptions{})
	require.NoError(t, err)
	cfg, err = LoadClient(path)
	require.NoError(t, err)
	require.Empty(t, cfg.TLS.SNI)
	require.True(t, cfg.TLS.Insecure)
}
