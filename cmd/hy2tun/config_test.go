package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hy2tun/internal/config"

	"github.com/stretchr/testify/require"
)

const cliValidConfig = `server: "host:443"
auth: "secret-auth"
control:
  token: "0123456789abcdef"
client:
  name: "router"
`

func writeCLIConfig(t *testing.T) string {
	t.Helper()
	return writeCLIConfigBody(t, cliValidConfig)
}

func writeCLIConfigBody(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	return path
}

// cliSetPayload builds a basic update for the valid CLI fixture. The server is
// unchanged so auth and control token are preserved.
func cliSetPayload(revision string) string {
	return fmt.Sprintf(`{"expected_revision":%q,"server":"host:443","client_name":"router","mappings":[],"web_listen":"","web_username":""}`, revision)
}

func cliRevision(t *testing.T, path string) string {
	t.Helper()
	revision, err := config.FileRevision(path)
	require.NoError(t, err)
	return revision
}

func TestRunConfigGetMissingReturnsDefaults(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runConfig([]string{"get", "-c", filepath.Join(t.TempDir(), "client.yaml")}, strings.NewReader(""), &out, &errOut)
	require.Equal(t, 0, code)
	require.Contains(t, out.String(), `"exists":false`)
	require.Empty(t, errOut.String())
}

func TestRunConfigGetParseErrorExitCode(t *testing.T) {
	path := writeCLIConfigBody(t, "server: [unterminated\n")
	var out, errOut bytes.Buffer
	code := runConfig([]string{"get", "-c", path}, strings.NewReader(""), &out, &errOut)
	require.Equal(t, 1, code)
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), `"ok":false`)
	require.Contains(t, errOut.String(), `"code":"parse_error"`)
}

func TestRunConfigGetInvalidConfigExitsZero(t *testing.T) {
	// Parseable but invalid configurations stay readable so LuCI can repair
	// them; only parse and IO failures are errors.
	path := writeCLIConfigBody(t, `server: "host:443"
control:
  token: "0123456789abcdef"
client:
  name: "router"
`)
	var out, errOut bytes.Buffer
	code := runConfig([]string{"get", "-c", path}, strings.NewReader(""), &out, &errOut)
	require.Equal(t, 0, code)
	require.Contains(t, out.String(), `"exists":true`)
	require.Contains(t, out.String(), `"valid":false`)
	require.Empty(t, errOut.String())
}

func TestRunConfigGetReadErrorExitCode(t *testing.T) {
	// A directory cannot be read as a config file. The config get contract
	// reports YAML parse and IO failures as parse_error with exit code 1.
	path := t.TempDir()
	var out, errOut bytes.Buffer
	code := runConfig([]string{"get", "-c", path}, strings.NewReader(""), &out, &errOut)
	require.Equal(t, 1, code)
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), `"code":"parse_error"`)
}

func TestRunConfigGetHidesSecrets(t *testing.T) {
	path := writeCLIConfig(t)
	var out, errOut bytes.Buffer
	code := runConfig([]string{"get", "-c", path}, strings.NewReader(""), &out, &errOut)
	require.Equal(t, 0, code)
	require.Contains(t, out.String(), `"auth_set":true`)
	require.Contains(t, out.String(), `"control_token_set":true`)
	require.NotContains(t, out.String(), "secret-auth")
	require.NotContains(t, out.String(), "0123456789abcdef")
}

func TestRunConfigSetConflictExitCode(t *testing.T) {
	path := writeCLIConfig(t)
	payload := `{"expected_revision":"wrong","server":"host:443","client_name":"r","mappings":[],"web_listen":"","web_username":""}`
	var out, errOut bytes.Buffer
	code := runConfig([]string{"set", "-c", path}, strings.NewReader(payload), &out, &errOut)
	require.Equal(t, 3, code)
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), `"code":"conflict"`)
}

func TestRunConfigSetRequireStopped(t *testing.T) {
	old := serviceRunning
	serviceRunning = func() bool { return true }
	t.Cleanup(func() { serviceRunning = old })

	path := writeCLIConfig(t)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	payload := `{"expected_revision":"...","server":"host:443","client_name":"r","mappings":[],"web_listen":"","web_username":""}`
	var out, errOut bytes.Buffer
	code := runConfig([]string{"set", "-c", path, "--require-stopped"}, strings.NewReader(payload), &out, &errOut)
	require.Equal(t, 6, code)
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), `"code":"service_running"`)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestRunConfigSetValidationExitCode(t *testing.T) {
	path := writeCLIConfig(t)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	payload := fmt.Sprintf(`{"expected_revision":%q,"server":"not-a-host","client_name":"router","mappings":[],"web_listen":"","web_username":""}`, cliRevision(t, path))
	var out, errOut bytes.Buffer
	code := runConfig([]string{"set", "-c", path}, strings.NewReader(payload), &out, &errOut)
	require.Equal(t, 2, code)
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), `"code":"validation"`)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestRunConfigSetUnknownFieldsExitCode(t *testing.T) {
	path := writeCLIConfigBody(t, cliValidConfig+"mystery: true\n")
	payload := fmt.Sprintf(`{"expected_revision":%q,"server":"host:443","client_name":"router","mappings":[],"web_listen":"","web_username":""}`, cliRevision(t, path))
	var out, errOut bytes.Buffer
	code := runConfig([]string{"set", "-c", path}, strings.NewReader(payload), &out, &errOut)
	require.Equal(t, 4, code)
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), `"code":"unknown_fields"`)
}

func TestRunConfigSetMultiExitCode(t *testing.T) {
	path := writeCLIConfigBody(t, `servers:
  - name: us
    server: "us.example.com:443"
    auth: "secret-us"
    control:
      token: "0123456789abcdef"
`)
	payload := fmt.Sprintf(`{"expected_revision":%q,"server":"host:443","client_name":"router","mappings":[],"web_listen":"","web_username":""}`, cliRevision(t, path))
	var out, errOut bytes.Buffer
	code := runConfig([]string{"set", "-c", path}, strings.NewReader(payload), &out, &errOut)
	require.Equal(t, 4, code)
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), `"code":"multi"`)
}

func TestRunConfigSetAdvancedExitCode(t *testing.T) {
	path := writeCLIConfigBody(t, cliValidConfig+"log:\n  level: verbose\n")
	payload := fmt.Sprintf(`{"expected_revision":%q,"server":"host:443","client_name":"router","mappings":[],"web_listen":"","web_username":""}`, cliRevision(t, path))
	var out, errOut bytes.Buffer
	code := runConfig([]string{"set", "-c", path}, strings.NewReader(payload), &out, &errOut)
	require.Equal(t, 5, code)
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), `"code":"advanced"`)
	require.Contains(t, errOut.String(), `"advanced":true`)
}

func TestRunConfigSetMalformedJSONExitCode(t *testing.T) {
	path := writeCLIConfig(t)
	var out, errOut bytes.Buffer
	code := runConfig([]string{"set", "-c", path}, strings.NewReader(`{"server":`), &out, &errOut)
	require.Equal(t, 1, code)
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), `"code":"parse_error"`)
}

func TestRunConfigSetUnknownJSONFieldExitCode(t *testing.T) {
	path := writeCLIConfig(t)
	payload := fmt.Sprintf(`{"expected_revision":%q,"server":"host:443","client_name":"router","mappings":[],"web_listen":"","web_username":"","surprise":true}`, cliRevision(t, path))
	var out, errOut bytes.Buffer
	code := runConfig([]string{"set", "-c", path}, strings.NewReader(payload), &out, &errOut)
	require.Equal(t, 1, code)
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), `"code":"parse_error"`)
}

func TestRunConfigSetSuccess(t *testing.T) {
	old := serviceRunning
	serviceRunning = func() bool { return false }
	t.Cleanup(func() { serviceRunning = old })

	path := writeCLIConfig(t)
	var out, errOut bytes.Buffer
	code := runConfig([]string{"set", "-c", path, "--require-stopped"}, strings.NewReader(cliSetPayload(cliRevision(t, path))), &out, &errOut)
	require.Equal(t, 0, code)
	require.Empty(t, errOut.String())

	var response struct {
		OK       bool   `json:"ok"`
		Revision string `json:"revision"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &response))
	require.True(t, response.OK)
	require.NotEmpty(t, response.Revision)
	require.Equal(t, cliRevision(t, path), response.Revision)

	saved, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(saved), "secret-auth")
	require.Contains(t, string(saved), "0123456789abcdef")
	require.NotContains(t, out.String(), "secret-auth")
}

func TestRunConfigUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := runConfig([]string{"bogus"}, strings.NewReader(""), &out, &errOut)
	require.Equal(t, 1, code)
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), "usage: hy2tun config")
}
