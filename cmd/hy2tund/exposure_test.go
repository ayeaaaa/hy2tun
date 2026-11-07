package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

const exposureTestPassword = "s3cret-web-password"

func writeExposureConfig(t *testing.T, listen string, withTLS bool) string {
	t.Helper()

	body := fmt.Sprintf(`listen: "127.0.0.1:443"
auth:
  type: password
  password: "server-auth-password"
control:
  token: "0123456789abcdef"
web:
  listen: %q
  username: "admin"
`, listen)
	if withTLS {
		body += `  tls:
    cert: "/tmp/web-cert.pem"
    key: "/tmp/web-key.pem"
`
	}
	path := filepath.Join(t.TempDir(), "server.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	return path
}

type exposureFakeState struct {
	LoginCalls       int
	APICalls         int
	LoginUsername    string
	LoginPassword    string
	LoginContentType string
	APIMethod        string
	APIPath          string
	APIOrigin        string
	APICSRF          string
	APICookie        string
	APIBody          string
}

type exposureFake struct {
	mu sync.Mutex

	loginStatus     int
	loginBody       string
	skipLoginCookie bool
	apiStatus       int
	apiBody         string

	state exposureFakeState
}

func newExposureFake(t *testing.T, withTLS bool) (*exposureFake, *httptest.Server) {
	t.Helper()

	fake := &exposureFake{
		loginStatus: http.StatusNoContent,
		apiStatus:   http.StatusOK,
		apiBody:     `{"Exposure":"public","UnavailableFamilies":null}`,
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/login":
			fake.handleLogin(w, r)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/clients/"):
			fake.handleAPI(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	var server *httptest.Server
	if withTLS {
		server = httptest.NewTLSServer(handler)
	} else {
		server = httptest.NewServer(handler)
	}
	// A deliberately untrusted certificate is part of the TLS test; do not
	// flood the test output with the expected handshake failure.
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	t.Cleanup(server.Close)
	return fake, server
}

func (f *exposureFake) handleLogin(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var payload struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	_ = json.Unmarshal(body, &payload)

	f.mu.Lock()
	f.state.LoginCalls++
	f.state.LoginUsername = payload.Username
	f.state.LoginPassword = payload.Password
	f.state.LoginContentType = r.Header.Get("Content-Type")
	status := f.loginStatus
	responseBody := f.loginBody
	skipCookie := f.skipLoginCookie
	f.mu.Unlock()

	if status < 200 || status >= 300 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if responseBody == "" {
			responseBody = `{"error":"login failed"}`
		}
		_, _ = io.WriteString(w, responseBody)
		return
	}
	if !skipCookie {
		http.SetCookie(w, &http.Cookie{
			Name:  "hy2tun_session",
			Value: "session-token",
			Path:  "/",
		})
	}
	w.WriteHeader(status)
}

func (f *exposureFake) handleAPI(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

	f.mu.Lock()
	f.state.APICalls++
	f.state.APIMethod = r.Method
	f.state.APIPath = r.URL.Path
	f.state.APIOrigin = r.Header.Get("Origin")
	f.state.APICSRF = r.Header.Get("X-Requested-With")
	if cookie, err := r.Cookie("hy2tun_session"); err == nil {
		f.state.APICookie = cookie.Value
	}
	f.state.APIBody = string(body)
	status := f.apiStatus
	responseBody := f.apiBody
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, responseBody)
}

func (f *exposureFake) setLoginResponse(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loginStatus = status
	f.loginBody = body
}

func (f *exposureFake) setAPIResponse(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.apiStatus = status
	f.apiBody = body
}

func (f *exposureFake) setLoginCookie(enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.skipLoginCookie = !enabled
}

func (f *exposureFake) snapshot() exposureFakeState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func exposureServerPort(t *testing.T, server *httptest.Server) string {
	t.Helper()

	parsed, err := url.Parse(server.URL)
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(parsed.Host)
	require.NoError(t, err)
	return port
}

func exposureArgs(configPath, clientID, mappingID, mode string, extra ...string) []string {
	args := []string{
		"--client", clientID,
		"--mapping", mappingID,
		"--mode", mode,
		"-c", configPath,
	}
	return append(args, extra...)
}

func assertExposureNoPassword(t *testing.T, password string, args []string, stdout, stderr string) {
	t.Helper()
	require.NotContains(t, strings.Join(args, " "), password)
	require.NotContains(t, stdout, password)
	require.NotContains(t, stderr, password)
}

func TestExposureCLIArgumentErrorsDoNotEchoValues(t *testing.T) {
	const mistyped = "mistyped-secret-value"
	cases := []struct {
		name string
		args []string
	}{
		{
			name: "positional_argument",
			args: []string{
				"--client", "c1",
				"--mapping", "m1",
				"--mode", "local",
				mistyped,
			},
		},
		{
			name: "unknown_flag",
			args: []string{
				"--client", "c1",
				"--mapping", "m1",
				"--mode", "local",
				"--" + mistyped,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			err := runExposure(tc.args, strings.NewReader(""), &stdout, &stderr)
			require.Error(t, err)
			require.Contains(t, err.Error(), "参数")
			require.NotContains(t, err.Error(), mistyped)
			require.NotContains(t, stdout.String(), mistyped)
			require.NotContains(t, stderr.String(), mistyped)
		})
	}
}

func TestExposureCLIHelp(t *testing.T) {
	var stdout, stderr strings.Builder
	err := runExposure([]string{"--help"}, strings.NewReader(""), &stdout, &stderr)
	require.NoError(t, err)
	require.Contains(t, stderr.String(), "Usage of exposure")
}

func TestExposureCLIParsesAndSends(t *testing.T) {
	fake, server := newExposureFake(t, false)
	fake.setAPIResponse(http.StatusOK, `{
		"id":"m1","type":"tcp","remote_port":2222,
		"local_addr":"127.0.0.1:22","note":"ssh","enabled":true,
		"Status":"running","Error":"","AssignedPort":2222,
		"Exposure":"local","UnavailableFamilies":["ipv6"],
		"TxBytes":0,"RxBytes":0
	}`)
	// An empty host is a wildcard bind. The CLI must connect to 127.0.0.1
	// first, not to the wildcard address.
	configPath := writeExposureConfig(t, ":"+exposureServerPort(t, server), false)
	args := exposureArgs(configPath, "c1", "m1", "local")

	var stdout, stderr strings.Builder
	err := runExposure(args, strings.NewReader(exposureTestPassword+"\n"), &stdout, &stderr)
	require.NoError(t, err)

	state := fake.snapshot()
	require.Equal(t, 1, state.LoginCalls)
	require.Equal(t, 1, state.APICalls)
	require.Equal(t, "admin", state.LoginUsername)
	require.Equal(t, exposureTestPassword, state.LoginPassword)
	require.True(t, strings.HasPrefix(state.LoginContentType, "application/json"))
	require.Equal(t, http.MethodPut, state.APIMethod)
	require.Equal(t, "/api/clients/c1/mappings/m1/exposure", state.APIPath)
	require.Equal(t, "http://"+server.Listener.Addr().String(), state.APIOrigin)
	require.Equal(t, "hy2tun", state.APICSRF)
	require.Equal(t, "session-token", state.APICookie)
	require.JSONEq(t, `{"exposure":"local"}`, state.APIBody)

	require.Contains(t, stdout.String(), "m1")
	require.Contains(t, stdout.String(), "local")
	require.Contains(t, stderr.String(), "IPv6 不可用")
	assertExposureNoPassword(t, exposureTestPassword, args, stdout.String(), stderr.String())
}

func TestExposureCLIPublicNoWarning(t *testing.T) {
	fake, server := newExposureFake(t, false)
	fake.setAPIResponse(http.StatusOK, `{
		"id":"m1","type":"tcp","remote_port":2222,
		"local_addr":"127.0.0.1:22","note":"ssh","enabled":true,
		"Status":"running","Error":"","AssignedPort":2222,
		"Exposure":"public","UnavailableFamilies":null,
		"TxBytes":0,"RxBytes":0
	}`)
	configPath := writeExposureConfig(t, server.Listener.Addr().String(), false)
	args := exposureArgs(configPath, "c1", "m1", "public")

	var stdout, stderr strings.Builder
	err := runExposure(args, strings.NewReader(exposureTestPassword+"\n"), &stdout, &stderr)
	require.NoError(t, err)

	state := fake.snapshot()
	require.Equal(t, 1, state.LoginCalls)
	require.Equal(t, 1, state.APICalls)
	require.Equal(t, "http://"+server.Listener.Addr().String(), state.APIOrigin)
	require.JSONEq(t, `{"exposure":"public"}`, state.APIBody)
	require.Contains(t, stdout.String(), "public")
	require.NotContains(t, stderr.String(), "警告")
	assertExposureNoPassword(t, exposureTestPassword, args, stdout.String(), stderr.String())
}

func TestExposureCLITrimsArguments(t *testing.T) {
	fake, server := newExposureFake(t, false)
	fake.setAPIResponse(http.StatusOK, `{
		"id":"m1","type":"tcp","remote_port":2222,
		"local_addr":"127.0.0.1:22","note":"ssh","enabled":true,
		"Status":"running","Error":"","AssignedPort":2222,
		"Exposure":"public","UnavailableFamilies":null,
		"TxBytes":0,"RxBytes":0
	}`)
	configPath := writeExposureConfig(t, server.Listener.Addr().String(), false)
	args := []string{
		"--client", " c1 ",
		"--mapping", " m1 ",
		"--mode", " public ",
		"-c", " " + configPath + " ",
	}

	var stdout, stderr strings.Builder
	err := runExposure(args, strings.NewReader(exposureTestPassword+"\n"), &stdout, &stderr)
	require.NoError(t, err)

	state := fake.snapshot()
	require.Equal(t, 1, state.LoginCalls)
	require.Equal(t, 1, state.APICalls)
	require.Equal(t, "/api/clients/c1/mappings/m1/exposure", state.APIPath)
	require.JSONEq(t, `{"exposure":"public"}`, state.APIBody)
	require.Contains(t, stdout.String(), "public")
	assertExposureNoPassword(t, exposureTestPassword, args, stdout.String(), stderr.String())
}

func TestExposureCLIPasswordLineEndings(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "lf", input: "secret\n", want: "secret"},
		{name: "crlf", input: "secret\r\n", want: "secret"},
		{name: "literal_trailing_cr", input: "secret\r", want: "secret\r"},
		{name: "double_cr", input: "secret\r\r\n", want: "secret\r"},
		{name: "no_terminator", input: "secret", want: "secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readExposurePassword(strings.NewReader(tc.input), io.Discard)
			require.NoError(t, err)
			require.Equal(t, []byte(tc.want), got)
			clear(got)
		})
	}
}

func TestExposureCLIRejectsMissingPassword(t *testing.T) {
	for name, input := range map[string]string{
		"empty_stdin": "",
		"blank_line":  "\n",
		"crlf_line":   "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			fake, server := newExposureFake(t, false)
			configPath := writeExposureConfig(t, server.Listener.Addr().String(), false)
			args := exposureArgs(configPath, "c1", "m1", "local")

			var stdout, stderr strings.Builder
			err := runExposure(args, strings.NewReader(input), &stdout, &stderr)
			require.Error(t, err)
			require.Contains(t, err.Error(), "密码")

			state := fake.snapshot()
			require.Zero(t, state.LoginCalls)
			require.Zero(t, state.APICalls)
			require.Empty(t, stdout.String())
			assertExposureNoPassword(t, exposureTestPassword, args, stdout.String(), stderr.String())
		})
	}
}

func TestExposureCLILoginFailure(t *testing.T) {
	fake, server := newExposureFake(t, false)
	fake.setLoginResponse(http.StatusUnauthorized, `{"error":"invalid credentials"}`)
	configPath := writeExposureConfig(t, server.Listener.Addr().String(), false)
	args := exposureArgs(configPath, "c1", "m1", "local")

	var stdout, stderr strings.Builder
	err := runExposure(args, strings.NewReader(exposureTestPassword+"\n"), &stdout, &stderr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "登录失败")
	require.Contains(t, err.Error(), "401")

	state := fake.snapshot()
	require.Equal(t, 1, state.LoginCalls)
	require.Zero(t, state.APICalls)
	require.Empty(t, stdout.String())
	assertExposureNoPassword(t, exposureTestPassword, args, stdout.String(), stderr.String())
}

func TestExposureCLILoginWithoutCookie(t *testing.T) {
	fake, server := newExposureFake(t, false)
	fake.setLoginCookie(false)
	configPath := writeExposureConfig(t, server.Listener.Addr().String(), false)
	args := exposureArgs(configPath, "c1", "m1", "local")

	var stdout, stderr strings.Builder
	err := runExposure(args, strings.NewReader(exposureTestPassword+"\n"), &stdout, &stderr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Cookie")

	state := fake.snapshot()
	require.Equal(t, 1, state.LoginCalls)
	require.Zero(t, state.APICalls)
	assertExposureNoPassword(t, exposureTestPassword, args, stdout.String(), stderr.String())
}

func TestExposureCLIWarningFormatting(t *testing.T) {
	cases := []struct {
		name     string
		families []string
		want     string
	}{
		{name: "none", families: nil, want: ""},
		{name: "ipv4", families: []string{"ipv4"}, want: "IPv4 不可用"},
		{name: "ipv6", families: []string{"ipv6"}, want: "IPv6 不可用"},
		{name: "both_reversed", families: []string{"ipv6", "ipv4"}, want: "IPv4/IPv6 不可用"},
		{name: "duplicate", families: []string{"ipv6", "IPV6"}, want: "IPv6 不可用"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, formatExposureWarning(tc.families))
		})
	}
}

func TestExposureCLIConflict(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantOutput []string
		wantNoWarn bool
	}{
		{
			name: "retained_active_exposure",
			body: `{
				"error":"映射暴露范围切换失败，已保留当前状态",
				"code":"exposure_conflict",
				"active_exposure":"public",
				"unavailable_families":["ipv6"],
				"closed":false
			}`,
			wantOutput: []string{"public", "IPv6 不可用"},
		},
		{
			name: "closed_mapping",
			body: `{
				"error":"映射暴露范围切换失败，映射已关闭",
				"code":"exposure_conflict",
				"active_exposure":"local",
				"unavailable_families":[],
				"closed":true
			}`,
			wantOutput: []string{"已关闭", "local"},
			wantNoWarn: true,
		},
		{
			name: "persistence_failure_retained",
			body: `{
				"error":"映射暴露范围切换失败，已保留当前状态",
				"code":"exposure_conflict",
				"active_exposure":"public",
				"unavailable_families":[],
				"closed":false
			}`,
			wantOutput: []string{"public"},
			wantNoWarn: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake, server := newExposureFake(t, false)
			fake.setAPIResponse(http.StatusConflict, tc.body)
			configPath := writeExposureConfig(t, server.Listener.Addr().String(), false)
			args := exposureArgs(configPath, "c1", "m1", "local")

			var stdout, stderr strings.Builder
			err := runExposure(args, strings.NewReader(exposureTestPassword+"\n"), &stdout, &stderr)
			require.Error(t, err)
			require.Contains(t, err.Error(), "失败")

			for _, want := range tc.wantOutput {
				require.Contains(t, stderr.String(), want)
			}
			if tc.wantNoWarn {
				require.NotContains(t, stderr.String(), "警告")
			}
			require.Empty(t, stdout.String())

			state := fake.snapshot()
			require.Equal(t, 1, state.APICalls)
			require.JSONEq(t, `{"exposure":"local"}`, state.APIBody)
			assertExposureNoPassword(t, exposureTestPassword, args, stdout.String(), stderr.String())
		})
	}
}

func TestExposureCLIResolveWebTargets(t *testing.T) {
	cases := []struct {
		name    string
		listen  string
		useTLS  bool
		want    []string
		wantErr string
	}{
		{
			name:   "empty_host_is_wildcard",
			listen: ":7501",
			want:   []string{"http://127.0.0.1:7501", "http://[::1]:7501"},
		},
		{
			name:   "ipv4_wildcard",
			listen: "0.0.0.0:7501",
			want:   []string{"http://127.0.0.1:7501", "http://[::1]:7501"},
		},
		{
			name:   "ipv6_wildcard",
			listen: "[::]:7501",
			want:   []string{"http://127.0.0.1:7501", "http://[::1]:7501"},
		},
		{
			name:   "specific_ipv4",
			listen: "127.0.0.1:7501",
			want:   []string{"http://127.0.0.1:7501"},
		},
		{
			name:   "specific_hostname",
			listen: "localhost:7501",
			want:   []string{"http://localhost:7501"},
		},
		{
			name:   "specific_ipv6",
			listen: "[::1]:7501",
			want:   []string{"http://[::1]:7501"},
		},
		{
			name:   "https_wildcard",
			listen: ":7501",
			useTLS: true,
			want:   []string{"https://127.0.0.1:7501", "https://[::1]:7501"},
		},
		{
			name:    "disabled",
			listen:  "",
			wantErr: "未启用",
		},
		{
			name:   "port_leading_zero",
			listen: ":07501",
			want:   []string{"http://127.0.0.1:7501", "http://[::1]:7501"},
		},
		{
			name:    "port_zero",
			listen:  ":0",
			wantErr: "端口",
		},
		{
			name:    "port_too_large",
			listen:  ":65536",
			wantErr: "端口",
		},
		{
			name:    "invalid",
			listen:  "not-a-host-port",
			wantErr: "无效",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveWebTargets(tc.listen, tc.useTLS)
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
				require.Nil(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestExposureCLIDoesNotUseEnvironmentProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("https_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	configPath := writeExposureConfig(t, "exposure-proxy.invalid:7501", false)
	args := exposureArgs(configPath, "c1", "m1", "local")
	var stdout, stderr strings.Builder
	err := runExposure(args, strings.NewReader(exposureTestPassword+"\n"), &stdout, &stderr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "连接 Web 控制台失败")
	require.Zero(t, proxyCalls.Load())
	assertExposureNoPassword(t, exposureTestPassword, args, stdout.String(), stderr.String())

	client, err := newExposureHTTPClient(false)
	require.NoError(t, err)
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.Nil(t, transport.Proxy)
}

func TestExposureCLIHTTPSInsecure(t *testing.T) {
	fake, server := newExposureFake(t, true)
	fake.setAPIResponse(http.StatusOK, `{
		"id":"m1","type":"tcp","remote_port":2222,
		"local_addr":"127.0.0.1:22","note":"ssh","enabled":true,
		"Status":"running","Error":"","AssignedPort":2222,
		"Exposure":"public","UnavailableFamilies":null,
		"TxBytes":0,"RxBytes":0
	}`)
	configPath := writeExposureConfig(t, server.Listener.Addr().String(), true)
	args := exposureArgs(configPath, "c1", "m1", "public")

	var stdout, stderr strings.Builder
	err := runExposure(args, strings.NewReader(exposureTestPassword+"\n"), &stdout, &stderr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--insecure")
	state := fake.snapshot()
	require.Zero(t, state.LoginCalls)
	require.Zero(t, state.APICalls)
	assertExposureNoPassword(t, exposureTestPassword, args, stdout.String(), stderr.String())

	stdout.Reset()
	stderr.Reset()
	args = append(args, "--insecure")
	err = runExposure(args, strings.NewReader(exposureTestPassword+"\n"), &stdout, &stderr)
	require.NoError(t, err)
	state = fake.snapshot()
	require.Equal(t, 1, state.LoginCalls)
	require.Equal(t, 1, state.APICalls)
	require.Equal(t, "https://"+server.Listener.Addr().String(), state.APIOrigin)
	require.Contains(t, stdout.String(), "public")
	assertExposureNoPassword(t, exposureTestPassword, args, stdout.String(), stderr.String())
}

func TestExposureCLIRejectsInsecureWithoutTLS(t *testing.T) {
	fake, server := newExposureFake(t, false)
	configPath := writeExposureConfig(t, server.Listener.Addr().String(), false)
	args := exposureArgs(configPath, "c1", "m1", "public", "--insecure")

	var stdout, stderr strings.Builder
	err := runExposure(args, strings.NewReader(exposureTestPassword+"\n"), &stdout, &stderr)
	require.Error(t, err)
	require.Contains(t, err.Error(), "--insecure")
	state := fake.snapshot()
	require.Zero(t, state.LoginCalls)
	require.Zero(t, state.APICalls)
}
