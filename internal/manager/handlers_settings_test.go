package manager

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	managerupstream "hy2tun/internal/manager/upstream"
)

type fakeSettingsLogin struct {
	Username string
	Password string
}

type fakeSettingsFailure struct {
	Status     int
	Code       string
	Message    string
	RetryAfter int
}

// fakeSettingsUpstream is a stateful hy2tund settings/credential API used by the
// Task 6 tests. It enforces the real login cookie, verifies the current Web
// administrator password, and records the exact JSON bodies the manager sends.
type fakeSettingsUpstream struct {
	server    *httptest.Server
	closeOnce sync.Once

	mu               sync.Mutex
	loginCount       int
	logins           []fakeSettingsLogin
	acceptedUsername string
	acceptedPassword string
	settings         managerupstream.Settings

	mutationAttempts int
	modeAttempts     int
	modeBodies       []map[string]any
	portsAttempts    int
	portsBodies      []map[string]any
	webAttempts      int
	webBodies        []map[string]any
	proxyAttempts    int
	proxyBodies      []map[string]any
	rotateAttempts   int
	rotateBodies     []map[string]any
	revokeAttempts   int
	revokeBodies     []string

	failures            map[string]fakeSettingsFailure
	rejectLogins        bool
	echoRotationSecrets bool
}

func newFakeSettingsUpstream(t *testing.T) *fakeSettingsUpstream {
	t.Helper()
	fake := &fakeSettingsUpstream{
		acceptedUsername: "admin",
		acceptedPassword: "upstream-pw",
		failures:         make(map[string]fakeSettingsFailure),
		settings: managerupstream.Settings{
			WebUsername:           "admin",
			WebPasswordConfigured: true,
			AuthType:              "password",
			TrustProxy:            false,
			Mode:                  config.ModeHY2,
			Ports: managerupstream.PortSettings{
				Allow:                 []string{"443"},
				Deny:                  []string{"22"},
				AutoRange:             "20000-29999",
				OfflineReserveSeconds: 30,
				OfflineReserve:        30 * time.Second,
			},
			Credentials: managerupstream.CredentialSettings{
				Generation:                 1,
				PreviousGeneration:         0,
				CurrentAuthMasked:          "********",
				CurrentControlTokenMasked:  "********",
				PreviousAuthMasked:         "",
				PreviousControlTokenMasked: "",
				Rotatable:                  true,
			},
			Clients: []managerupstream.ClientCredential{
				{ClientID: "c1", Name: "laptop", Online: true, Generation: 1, Status: "synced", LastSeen: time.Now().UTC()},
				{ClientID: "c2", Name: "phone", Online: false, Generation: 1, Status: "pending", LastSeen: time.Now().UTC().Add(-time.Hour)},
			},
			ReadOnly: []managerupstream.ReadOnlySetting{
				{Label: "监听地址", Value: ":443"},
			},
		},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.close)
	return fake
}

func (f *fakeSettingsUpstream) BaseURL() string { return f.server.URL + fakeUpstreamBasePath }

func (f *fakeSettingsUpstream) close() { f.closeOnce.Do(f.server.Close) }

func (f *fakeSettingsUpstream) setEchoRotationSecrets(echo bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.echoRotationSecrets = echo
}

func (f *fakeSettingsUpstream) setRejectLogins(reject bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejectLogins = reject
}

func (f *fakeSettingsUpstream) setFailure(kind string, status int, code, message string, retryAfter int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[kind] = fakeSettingsFailure{
		Status: status, Code: code, Message: message, RetryAfter: retryAfter,
	}
}

func (f *fakeSettingsUpstream) loginAttempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginCount
}

func (f *fakeSettingsUpstream) lastLogin() fakeSettingsLogin {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.logins) == 0 {
		return fakeSettingsLogin{}
	}
	return f.logins[len(f.logins)-1]
}

func (f *fakeSettingsUpstream) mutationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mutationAttempts
}

func (f *fakeSettingsUpstream) attempts(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch kind {
	case "mode":
		return f.modeAttempts
	case "ports":
		return f.portsAttempts
	case "web":
		return f.webAttempts
	case "proxy":
		return f.proxyAttempts
	case "rotate":
		return f.rotateAttempts
	case "revoke":
		return f.revokeAttempts
	default:
		return 0
	}
}

func (f *fakeSettingsUpstream) bodies(kind string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var source []map[string]any
	switch kind {
	case "mode":
		source = f.modeBodies
	case "ports":
		source = f.portsBodies
	case "web":
		source = f.webBodies
	case "proxy":
		source = f.proxyBodies
	case "rotate":
		source = f.rotateBodies
	}
	out := make([]map[string]any, 0, len(source))
	for _, body := range source {
		cloned := make(map[string]any, len(body))
		for key, value := range body {
			cloned[key] = value
		}
		out = append(out, cloned)
	}
	return out
}

func (f *fakeSettingsUpstream) revokePayloads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.revokeBodies))
	copy(out, f.revokeBodies)
	return out
}

func (f *fakeSettingsUpstream) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, fakeUpstreamBasePath+"/") {
		http.NotFound(w, r)
		return
	}
	path = strings.TrimPrefix(path, fakeUpstreamBasePath)

	if path == "/api/login" {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": "bad form"})
			return
		}
		username := r.PostForm.Get("username")
		password := r.PostForm.Get("password")
		f.mu.Lock()
		f.loginCount++
		f.logins = append(f.logins, fakeSettingsLogin{Username: username, Password: password})
		accepted := username == f.acceptedUsername && password == f.acceptedPassword && !f.rejectLogins
		f.mu.Unlock()
		if !accepted {
			writeTestJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "invalid credentials", "code": "unauthorized",
			})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "hy2tun_session", Value: "fake-session", Path: "/"})
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if cookie, err := r.Cookie("hy2tun_session"); err != nil || cookie.Value != "fake-session" {
		writeTestJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized", "code": "unauthorized"})
		return
	}

	switch path {
	case "/api/overview":
		writeTestJSON(w, http.StatusOK, map[string]any{
			"online_clients": 2, "active_mappings": 5,
			"listen": ":443", "uptime_seconds": 3600,
		})
		return
	case "/api/config/summary":
		f.mu.Lock()
		mode := f.settings.Mode
		f.mu.Unlock()
		writeTestJSON(w, http.StatusOK, map[string]any{
			"version": "v1.7.4", "mode": mode, "listen": ":443",
		})
		return
	case "/api/settings":
		f.mu.Lock()
		settings := f.settings
		settings.Ports.OfflineReserve = time.Duration(settings.Ports.OfflineReserveSeconds) * time.Second
		f.mu.Unlock()
		writeTestJSON(w, http.StatusOK, settings)
		return
	}

	switch path {
	case "/api/settings/mode":
		if r.Method != http.MethodPut {
			f.methodNotAllowed(w)
			return
		}
		body := f.beginMutation(w, r, "mode")
		if body == nil {
			return
		}
		mode, _ := body["mode"].(string)
		f.mu.Lock()
		f.modeBodies = append(f.modeBodies, body)
		f.settings.Mode = config.TransportMode(mode)
		f.mu.Unlock()
		writeTestJSON(w, http.StatusOK, map[string]any{"mode": mode})
	case "/api/settings/ports":
		if r.Method != http.MethodPut {
			f.methodNotAllowed(w)
			return
		}
		body := f.beginMutation(w, r, "ports")
		if body == nil {
			return
		}
		f.mu.Lock()
		f.portsBodies = append(f.portsBodies, body)
		f.settings.Ports.Allow = stringSlice(body["allow"])
		f.settings.Ports.Deny = stringSlice(body["deny"])
		f.settings.Ports.AutoRange, _ = body["auto_range"].(string)
		if seconds, ok := body["offline_reserve_seconds"].(float64); ok {
			f.settings.Ports.OfflineReserveSeconds = int64(seconds)
			f.settings.Ports.OfflineReserve = time.Duration(int64(seconds)) * time.Second
		}
		settings := f.settings
		settings.Ports.OfflineReserve = time.Duration(settings.Ports.OfflineReserveSeconds) * time.Second
		f.mu.Unlock()
		writeTestJSON(w, http.StatusOK, settings)
	case "/api/settings/web":
		if r.Method != http.MethodPut {
			f.methodNotAllowed(w)
			return
		}
		body := f.beginMutation(w, r, "web")
		if body == nil {
			return
		}
		var payload struct {
			Username        string `json:"username"`
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
			ConfirmPassword string `json:"confirm_password"`
		}
		raw, _ := json.Marshal(body)
		_ = json.Unmarshal(raw, &payload)
		f.mu.Lock()
		if payload.CurrentPassword != f.acceptedPassword {
			f.mu.Unlock()
			writeTestJSON(w, http.StatusForbidden, map[string]any{
				"error": "当前密码不正确", "code": "invalid_password",
			})
			return
		}
		f.webBodies = append(f.webBodies, body)
		f.acceptedUsername = payload.Username
		f.acceptedPassword = payload.NewPassword
		f.settings.WebUsername = payload.Username
		f.settings.WebPasswordConfigured = true
		f.mu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	case "/api/settings/proxy":
		if r.Method != http.MethodPut {
			f.methodNotAllowed(w)
			return
		}
		body := f.beginMutation(w, r, "proxy")
		if body == nil {
			return
		}
		trust, _ := body["trust_proxy"].(bool)
		f.mu.Lock()
		f.proxyBodies = append(f.proxyBodies, body)
		f.settings.TrustProxy = trust
		settings := f.settings
		f.mu.Unlock()
		writeTestJSON(w, http.StatusOK, settings)
	case "/api/settings/credentials":
		if r.Method != http.MethodPut {
			f.methodNotAllowed(w)
			return
		}
		body := f.beginMutation(w, r, "rotate")
		if body == nil {
			return
		}
		f.mu.Lock()
		f.rotateBodies = append(f.rotateBodies, body)
		previous := f.settings.Credentials.Generation
		if previous < 1 {
			previous = 1
		}
		next := previous + 1
		f.settings.Credentials.Generation = next
		f.settings.Credentials.PreviousGeneration = previous
		f.settings.Credentials.CurrentAuthMasked = "********"
		f.settings.Credentials.CurrentControlTokenMasked = "********"
		f.settings.Credentials.PreviousAuthMasked = "********"
		f.settings.Credentials.PreviousControlTokenMasked = "********"
		for index := range f.settings.Clients {
			f.settings.Clients[index].Generation = next
			if f.settings.Clients[index].Online {
				f.settings.Clients[index].Status = "synced"
			} else {
				f.settings.Clients[index].Status = "pending"
			}
			f.settings.Clients[index].Error = ""
		}
		results := []managerupstream.CredentialResult{
			{ClientID: "c1", Name: "laptop", Generation: next, Status: "synced"},
			{ClientID: "c2", Name: "phone", Generation: previous, Status: "pending"},
		}
		if f.echoRotationSecrets {
			auth, _ := body["auth"].(string)
			token, _ := body["control_token"].(string)
			results[0].Error = "push failed: " + auth + " / " + token
		}
		f.mu.Unlock()
		writeTestJSON(w, http.StatusOK, map[string]any{"generation": next, "results": results})
	case "/api/settings/credentials/revoke":
		if r.Method != http.MethodPost {
			f.methodNotAllowed(w)
			return
		}
		body := f.beginMutation(w, r, "revoke")
		if body == nil {
			return
		}
		raw, _ := json.Marshal(body)
		f.mu.Lock()
		f.revokeBodies = append(f.revokeBodies, string(raw))
		f.settings.Credentials.PreviousGeneration = 0
		f.settings.Credentials.PreviousAuthMasked = ""
		f.settings.Credentials.PreviousControlTokenMasked = ""
		f.mu.Unlock()
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	default:
		writeTestJSON(w, http.StatusNotFound, map[string]any{"error": "not found", "code": "not_found"})
	}
}

func (f *fakeSettingsUpstream) methodNotAllowed(w http.ResponseWriter) {
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (f *fakeSettingsUpstream) beginMutation(w http.ResponseWriter, r *http.Request, kind string) map[string]any {
	body := map[string]any{}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": "read body", "code": "invalid_body"})
		return nil
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body", "code": "invalid_body"})
			return nil
		}
	}

	f.mu.Lock()
	f.mutationAttempts++
	switch kind {
	case "mode":
		f.modeAttempts++
	case "ports":
		f.portsAttempts++
	case "web":
		f.webAttempts++
	case "proxy":
		f.proxyAttempts++
	case "rotate":
		f.rotateAttempts++
	case "revoke":
		f.revokeAttempts++
	}
	failure, failing := f.failures[kind]
	f.mu.Unlock()
	if failing {
		if failure.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(failure.RetryAfter))
		}
		writeTestJSON(w, failure.Status, map[string]any{
			"error": failure.Message, "code": failure.Code,
		})
		return nil
	}
	return body
}

func stringSlice(value any) []string {
	values, ok := value.([]any)
	if !ok {
		if stringsList, ok := value.([]string); ok {
			return append([]string(nil), stringsList...)
		}
		return nil
	}
	out := make([]string, 0, len(values))
	for _, item := range values {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func addFakeSettingsServer(t *testing.T, env *managerTestEnv, upstream *fakeSettingsUpstream) ServerView {
	t.Helper()
	resp, body := env.postJSON(t, "/api/servers", addServerPayload("us", upstream.BaseURL(), "upstream-pw"))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var server ServerView
	require.NoError(t, json.Unmarshal([]byte(body), &server))
	return server
}

func settingsResponse(t *testing.T, body string) managerupstream.Settings {
	t.Helper()
	var settings managerupstream.Settings
	require.NoError(t, json.Unmarshal([]byte(body), &settings))
	return settings
}

func settingsActionResults(entries []AuditEntry, action string) []string {
	results := make([]string, 0, 2)
	for _, entry := range entries {
		if entry.Action == action {
			results = append(results, entry.Result)
		}
	}
	return results
}

func TestServerSettingsMutationsForwardAndAudit(t *testing.T) {
	upstream := newFakeSettingsUpstream(t)
	env := newManagerTestEnv(t)
	server := addFakeSettingsServer(t, env, upstream)
	base := "/api/servers/" + server.ID + "/settings"

	status, page := env.getPage(t, "/servers/"+server.ID+"/settings")
	require.Equal(t, http.StatusOK, status, page)
	require.Contains(t, page, "传输模式")
	require.Contains(t, page, "端口策略")
	require.Contains(t, page, "Web 管理员")
	require.Contains(t, page, "信任反向代理")
	require.Contains(t, page, "HY2 凭据")
	require.Contains(t, page, "20000-29999")
	require.Contains(t, page, "laptop")
	require.Contains(t, page, "已同步")
	require.Contains(t, page, "********")
	require.Contains(t, page, "监听地址")
	require.NotContains(t, page, "upstream-pw")

	resp := env.request(t, http.MethodGet, base, "")
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, `"web_username"`)
	require.NotContains(t, body, "upstream-pw")

	// Mode.
	resp, body = env.putJSON(t, base+"/mode", map[string]any{"mode": "tcp"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Equal(t, config.ModeTCP, settingsResponse(t, body).Mode)
	require.Equal(t, []map[string]any{{"mode": "tcp"}}, upstream.bodies("mode"))

	// Ports.
	resp, body = env.putJSON(t, base+"/ports", map[string]any{
		"allow":                   []string{"443", "8000-8100"},
		"deny":                    []string{"22"},
		"auto_range":              "20000-30000",
		"offline_reserve_seconds": 45,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	ports := settingsResponse(t, body).Ports
	require.Equal(t, []string{"443", "8000-8100"}, ports.Allow)
	require.Equal(t, []string{"22"}, ports.Deny)
	require.Equal(t, "20000-30000", ports.AutoRange)
	require.Equal(t, int64(45), ports.OfflineReserveSeconds)
	portBodies := upstream.bodies("ports")
	require.Len(t, portBodies, 1)
	require.Equal(t, float64(45), portBodies[0]["offline_reserve_seconds"])
	require.Equal(t, []any{"443", "8000-8100"}, portBodies[0]["allow"])

	// Trust proxy.
	resp, body = env.putJSON(t, base+"/trust-proxy", map[string]any{"trust_proxy": true})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.True(t, settingsResponse(t, body).TrustProxy)
	require.Equal(t, []map[string]any{{"trust_proxy": true}}, upstream.bodies("proxy"))

	// Credential rotation.
	rotatePayload := map[string]any{
		"auth":          "new-auth-pass",
		"control_token": "new-control-token-123",
		"sync_mode":     "auto",
	}
	resp, body = env.putJSON(t, base+"/credentials", rotatePayload)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var rotated struct {
		managerupstream.Settings
		Generation int64                              `json:"generation"`
		Results    []managerupstream.CredentialResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &rotated))
	require.Equal(t, int64(2), rotated.Generation)
	require.Equal(t, int64(2), rotated.Settings.Credentials.Generation)
	require.Len(t, rotated.Results, 2)
	require.Equal(t, "synced", rotated.Results[0].Status)
	require.Equal(t, []map[string]any{rotatePayload}, upstream.bodies("rotate"))

	// Revoke requires explicit confirmation and must not reach upstream
	// without it.
	resp, body = env.postJSON(t, base+"/credentials/revoke", map[string]any{})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	require.Zero(t, upstream.attempts("revoke"))

	resp, body = env.postJSON(t, base+"/credentials/revoke?confirm=revoke", map[string]any{})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Zero(t, settingsResponse(t, body).Credentials.PreviousGeneration)
	require.Equal(t, []string{"{}"}, upstream.revokePayloads())

	// A request without the manager CSRF header must not reach upstream.
	noCSRF, err := http.NewRequest(http.MethodPut, env.server.URL+base+"/mode", strings.NewReader(`{"mode":"hy2"}`))
	require.NoError(t, err)
	noCSRF.AddCookie(env.cookie)
	noCSRF.Header.Set("Content-Type", "application/json")
	resp, err = env.server.Client().Do(noCSRF)
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, 1, upstream.attempts("mode"), "CSRF rejection must not reach upstream")

	entries := auditEntries(t, env.audit)
	for _, action := range []string{
		"settings.mode", "settings.ports", "settings.trust_proxy",
		"settings.credentials.rotate", "settings.credentials.revoke",
	} {
		require.Equal(t, []string{"pending", "ok"}, settingsActionResults(entries, action), action)
	}
	rawAudit := readAuditRaw(t, env.audit)
	for _, secret := range []string{"upstream-pw", "new-auth-pass", "new-control-token-123"} {
		require.NotContains(t, rawAudit, secret)
	}
}

func TestWebAdminPasswordUpdateUpdatesStoredSecret(t *testing.T) {
	upstream := newFakeSettingsUpstream(t)
	env := newManagerTestEnv(t)
	server := addFakeSettingsServer(t, env, upstream)

	var oldClient *managerupstream.Client
	require.NoError(t, env.inventory.WithClient(context.Background(), server.ID,
		func(_ context.Context, client *managerupstream.Client) error {
			oldClient = client
			return nil
		}))
	require.NotNil(t, oldClient)

	beforeLogins := upstream.loginAttempts()
	resp, body := env.putJSON(t, "/api/servers/"+server.ID+"/settings/web", map[string]any{
		"username":         "ops",
		"new_password":     "next-secret-pw",
		"confirm_password": "next-secret-pw",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.NotContains(t, body, "upstream-pw")
	require.NotContains(t, body, "next-secret-pw")

	response := settingsResponse(t, body)
	require.Equal(t, "ops", response.WebUsername)
	require.True(t, response.WebPasswordConfigured)

	stored, err := env.store.ServerPassword(server.ID)
	require.NoError(t, err)
	require.Equal(t, "next-secret-pw", string(stored))
	require.NotContains(t, storeRaw(t, env.inventory), "upstream-pw")
	require.NotContains(t, storeRaw(t, env.inventory), "next-secret-pw")

	webBodies := upstream.bodies("web")
	require.Len(t, webBodies, 1)
	require.Equal(t, "ops", webBodies[0]["username"])
	require.Equal(t, "upstream-pw", webBodies[0]["current_password"])
	require.Equal(t, "next-secret-pw", webBodies[0]["new_password"])
	require.Equal(t, "next-secret-pw", webBodies[0]["confirm_password"])

	require.Greater(t, upstream.loginAttempts(), beforeLogins, "the manager must re-authenticate after the password change")
	last := upstream.lastLogin()
	require.Equal(t, "ops", last.Username)
	require.Equal(t, "next-secret-pw", last.Password)

	var newClient *managerupstream.Client
	require.NoError(t, env.inventory.WithClient(context.Background(), server.ID,
		func(ctx context.Context, client *managerupstream.Client) error {
			newClient = client
			_, err := client.Settings(ctx)
			return err
		}))
	require.NotNil(t, newClient)
	require.NotSame(t, oldClient, newClient, "the next WithClient must recreate the upstream client from the new ciphertext")

	entries := auditEntries(t, env.audit)
	require.Equal(t, []string{"pending", "ok"}, settingsActionResults(entries, "settings.web"))
	rawAudit := readAuditRaw(t, env.audit)
	require.NotContains(t, rawAudit, "upstream-pw")
	require.NotContains(t, rawAudit, "next-secret-pw")
}

func TestWebAdminReloginFailureMarksAuthErrorAndKeepsNewSecret(t *testing.T) {
	upstream := newFakeSettingsUpstream(t)
	env := newManagerTestEnv(t)
	server := addFakeSettingsServer(t, env, upstream)

	// The upstream accepts the Web administrator change but then rejects the
	// new login. The manager must keep the new encrypted password because the
	// old one is no longer valid and mark the server auth_error.
	upstream.setRejectLogins(true)
	resp, body := env.putJSON(t, "/api/servers/"+server.ID+"/settings/web", map[string]any{
		"username":         "ops",
		"new_password":     "next-secret-pw",
		"confirm_password": "next-secret-pw",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, `"applied":true`)
	require.Contains(t, body, `"settings_available":false`)
	require.Contains(t, body, "重新登录失败")
	require.NotContains(t, body, `"web_username"`)
	require.NotContains(t, body, `"mode"`)
	require.NotContains(t, body, "next-secret-pw")
	require.NotContains(t, body, "upstream-pw")

	stored, err := env.store.ServerPassword(server.ID)
	require.NoError(t, err)
	require.Equal(t, "next-secret-pw", string(stored))

	view, ok := env.inventory.Get(server.ID)
	require.True(t, ok)
	require.Equal(t, ServerStateAuthError, view.State)

	entries := auditEntries(t, env.audit)
	require.Equal(t, []string{"pending", "ok"}, settingsActionResults(entries, "settings.web"))
	relogin := settingsActionResults(entries, "settings.web.relogin")
	require.Equal(t, []string{"error"}, relogin)
	rawAudit := readAuditRaw(t, env.audit)
	require.NotContains(t, rawAudit, "upstream-pw")
	require.NotContains(t, rawAudit, "next-secret-pw")
}

func TestSettingsSecretSanitizersDoNotLeak(t *testing.T) {
	settings := sanitizeSettingsSnapshot(managerupstream.Settings{
		Credentials: managerupstream.CredentialSettings{
			CurrentAuthMasked:          "plain-auth",
			CurrentControlTokenMasked:  "plain-token",
			PreviousAuthMasked:         "old-auth",
			PreviousControlTokenMasked: "old-token",
		},
	})
	require.Equal(t, "********", settings.Credentials.CurrentAuthMasked)
	require.Equal(t, "********", settings.Credentials.CurrentControlTokenMasked)
	require.Equal(t, "********", settings.Credentials.PreviousAuthMasked)
	require.Equal(t, "********", settings.Credentials.PreviousControlTokenMasked)

	results := sanitizeCredentialResults([]managerupstream.CredentialResult{
		{ClientID: "c1", Error: "push plain-auth plain-token"},
		{ClientID: "c2", Error: "ok"},
	}, "plain-auth", "plain-token")
	require.NotContains(t, results[0].Error, "plain-auth")
	require.NotContains(t, results[0].Error, "plain-token")
	require.Equal(t, "ok", results[1].Error)
}

func TestWebAdminUpdateUsesCurrentInventoryRecordUnderLock(t *testing.T) {
	upstream := newFakeSettingsUpstream(t)
	env := newManagerTestEnv(t)
	server := addFakeSettingsServer(t, env, upstream)

	// Simulate an inventory edit that completes after the handler read its
	// outer record but before it acquires the per-server operation lock.
	lock := env.inventory.operationLock(server.ID)
	lock.Lock()
	released := false
	defer func() {
		if !released {
			lock.Unlock()
		}
	}()

	body, err := json.Marshal(map[string]any{
		"username":         "ops",
		"new_password":     "next-secret-pw",
		"confirm_password": "next-secret-pw",
	})
	require.NoError(t, err)
	request := env.newRequest(t, http.MethodPut, "/api/servers/"+server.ID+"/settings/web", string(body))

	type requestResult struct {
		status int
		body   string
		err    error
	}
	done := make(chan requestResult, 1)
	go func() {
		response, requestErr := env.server.Client().Do(request)
		if requestErr != nil {
			done <- requestResult{err: requestErr}
			return
		}
		defer response.Body.Close()
		data, readErr := io.ReadAll(response.Body)
		done <- requestResult{status: response.StatusCode, body: string(data), err: readErr}
	}()

	require.Eventually(t, func() bool {
		return auditContainsPending(env.audit.path, "settings.web")
	}, 2*time.Second, 20*time.Millisecond, "web settings handler did not append its pending audit entry")

	current := findSettingsServerRecordByID(t, env.store, server.ID)
	current.Name = "concurrent-edit"
	require.NoError(t, env.store.UpdateServer(current, nil))
	lock.Unlock()
	released = true

	var result requestResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("web settings request did not finish")
	}
	require.NoError(t, result.err)
	require.Equal(t, http.StatusOK, result.status, result.body)

	updated := findSettingsServerRecordByID(t, env.store, server.ID)
	require.Equal(t, "concurrent-edit", updated.Name, "a concurrent inventory name edit must not be clobbered")
	require.Equal(t, "ops", updated.UpstreamUsername)
	require.Equal(t, current.BaseURL, updated.BaseURL)
	require.Equal(t, current.TLSSkipVerify, updated.TLSSkipVerify)

	entries := auditEntries(t, env.audit)
	var terminal *AuditEntry
	for index := range entries {
		if entries[index].Action == settingsWebAction && entries[index].Result == "ok" {
			terminal = &entries[index]
		}
	}
	require.NotNil(t, terminal)
	require.Equal(t, "concurrent-edit", terminal.ServerName, "terminal audit should use the current inventory name")
}

func auditContainsPending(path, action string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry AuditEntry
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry.Action == action && entry.Result == "pending" {
			return true
		}
	}
	return false
}

func findSettingsServerRecordByID(t *testing.T, store *Store, id string) ServerRecord {
	t.Helper()
	for _, record := range store.State().Servers {
		if record.ID == id {
			return record
		}
	}
	t.Fatalf("server %q not found in state", id)
	return ServerRecord{}
}

func TestRotationClientResultsAreSanitizedInJSONHTMLAndAudit(t *testing.T) {
	upstream := newFakeSettingsUpstream(t)
	upstream.setEchoRotationSecrets(true)
	env := newManagerTestEnv(t)
	server := addFakeSettingsServer(t, env, upstream)
	base := "/api/servers/" + server.ID + "/settings"

	jsonAuth := "rotation-json-auth"
	jsonToken := "rotation-json-token-0123456789"
	resp, body := env.putJSON(t, base+"/credentials", map[string]any{
		"auth": jsonAuth, "control_token": jsonToken, "sync_mode": "auto",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, "[REDACTED]")
	require.NotContains(t, body, jsonAuth)
	require.NotContains(t, body, jsonToken)
	var rotated struct {
		Results []managerupstream.CredentialResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &rotated))
	require.NotEmpty(t, rotated.Results)
	require.Contains(t, rotated.Results[0].Error, "[REDACTED]")

	htmlAuth := "rotation-html-auth"
	htmlToken := "rotation-html-token-0123456789"
	resp, body = env.putForm(t, base+"/credentials", url.Values{
		"auth":          {htmlAuth},
		"control_token": {htmlToken},
		"sync_mode":     {"auto"},
	}, true)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, "[REDACTED]")
	require.NotContains(t, body, htmlAuth)
	require.NotContains(t, body, htmlToken)

	rawAudit := readAuditRaw(t, env.audit)
	for _, secret := range []string{jsonAuth, jsonToken, htmlAuth, htmlToken} {
		require.NotContains(t, rawAudit, secret)
	}
}

func TestFailedFormParsePreservesCurrentSettingValues(t *testing.T) {
	upstream := newFakeSettingsUpstream(t)
	env := newManagerTestEnv(t)
	server := addFakeSettingsServer(t, env, upstream)

	hugeAllow := strings.Repeat("x", settingsBodyLimit+1024)
	resp, body := env.putForm(t, "/api/servers/"+server.ID+"/settings/ports", url.Values{
		"allow": {hugeAllow},
	}, true)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	require.Contains(t, body, "请求体不是有效的表单")
	require.Contains(t, body, `value="20000-29999"`, "unrelated settings must keep their current values")
	require.Contains(t, body, `value="30"`, "unrelated settings must keep their current values")
	require.NotContains(t, body, strings.Repeat("x", 128))
}

func TestSettingsSuccessNoticeOnlyClaimsRefreshWhenReadSucceeded(t *testing.T) {
	require.Equal(t, "服务器设置已保存，但未能读取最新配置。", settingsSuccessNotice(false, ""))
	require.Empty(t, settingsSuccessNotice(false, "设置已保存，但读取失败"))
	require.Empty(t, settingsSuccessNotice(true, "设置已保存，但读取失败"))
	require.Equal(t, "服务器设置已保存，并已重新读取最新配置。", settingsSuccessNotice(true, ""))
}

func TestSettingsFailuresAreAuditedAndNoSecretLeak(t *testing.T) {
	upstream := newFakeSettingsUpstream(t)
	env := newManagerTestEnv(t)
	server := addFakeSettingsServer(t, env, upstream)
	base := "/api/servers/" + server.ID + "/settings"

	upstream.setFailure("web", http.StatusForbidden, "invalid_password", "当前密码不正确", 0)
	resp, body := env.putJSON(t, base+"/web", map[string]any{
		"username":         "ops",
		"new_password":     "brand-new-secret",
		"confirm_password": "brand-new-secret",
	})
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, body)
	require.Contains(t, body, "invalid_password")
	require.Contains(t, body, "当前上游 Web 管理员密码不正确")
	require.NotContains(t, body, "upstream-pw")
	require.NotContains(t, body, "brand-new-secret")

	stored, err := env.store.ServerPassword(server.ID)
	require.NoError(t, err)
	require.Equal(t, "upstream-pw", string(stored), "a rejected current password must not change the stored credential")

	upstream.setFailure("rotate", http.StatusUnprocessableEntity, "invalid_credentials", "auth password must be at least 8 bytes", 0)
	resp, body = env.putJSON(t, base+"/credentials", map[string]any{
		"auth":          "new-auth-secret",
		"control_token": "new-control-token-secret",
		"sync_mode":     "auto",
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	require.Contains(t, body, "invalid_credentials")
	require.NotContains(t, body, "new-auth-secret")
	require.NotContains(t, body, "new-control-token-secret")
	require.NotContains(t, body, "auth password must be at least 8 bytes", "raw upstream detail must not be rendered")

	upstream.setFailure("rotate", http.StatusTooManyRequests, "rate_limited", "请求过多", 30)
	resp, body = env.putJSON(t, base+"/credentials", map[string]any{
		"auth":          "new-auth-secret",
		"control_token": "new-control-token-secret",
		"sync_mode":     "auto",
	})
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, body)
	require.Equal(t, "30", resp.Header.Get("Retry-After"))
	require.Contains(t, body, "30 秒后可重试")
	require.Equal(t, 2, upstream.attempts("rotate"), "a rate-limited rotation must be attempted exactly once and never retried automatically")

	// A form-encoded failure is rendered as HTML with the operator's
	// non-secret input preserved and secret fields never echoed.
	upstream.setFailure("ports", http.StatusUnprocessableEntity, "invalid_ports", "raw-upstream-port-detail", 0)
	resp, body = env.putForm(t, base+"/ports", url.Values{
		"allow":                   {"443, 8000-8100"},
		"deny":                    {"22"},
		"auto_range":              {"20000-30000"},
		"offline_reserve_seconds": {"45"},
	}, true)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	require.Contains(t, body, "端口策略无效")
	require.NotContains(t, body, "raw-upstream-port-detail", "raw upstream detail must not be rendered")
	require.Contains(t, body, "443, 8000-8100", "failed form input must be preserved")
	require.Contains(t, body, `value="45"`)

	upstream.setFailure("revoke", http.StatusUnprocessableEntity, "revoke_failed", "raw-revoke-detail", 0)
	resp, body = env.postJSON(t, base+"/credentials/revoke?confirm=revoke", map[string]any{})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
	require.Contains(t, body, "撤销旧凭据失败")
	require.NotContains(t, body, "raw-revoke-detail", "raw upstream detail must not be rendered")

	upstream.setFailure("web", http.StatusForbidden, "invalid_password", "当前密码不正确", 0)
	resp, body = env.putForm(t, base+"/web", url.Values{
		"username":         {"ops2"},
		"new_password":     {"form-secret-password"},
		"confirm_password": {"form-secret-password"},
	}, true)
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, body)
	require.Contains(t, body, "当前上游 Web 管理员密码不正确")
	require.Contains(t, body, `value="ops2"`)
	require.NotContains(t, body, "form-secret-password")
	require.NotContains(t, body, "upstream-pw")

	entries := auditEntries(t, env.audit)
	webResults := settingsActionResults(entries, "settings.web")
	require.Equal(t, []string{"pending", "error", "pending", "error"}, webResults)
	for _, entry := range entries {
		if entry.Result == "pending" {
			continue
		}
		switch entry.Action {
		case "settings.web":
			require.Equal(t, "invalid_password", entry.Code)
		case "settings.credentials.rotate":
			require.Contains(t, []string{"invalid_credentials", "rate_limited"}, entry.Code)
		}
	}
	rawAudit := readAuditRaw(t, env.audit)
	for _, secret := range []string{
		"upstream-pw", "brand-new-secret", "new-auth-secret",
		"new-control-token-secret", "form-secret-password",
		"auth password must be at least 8 bytes", "raw-upstream-port-detail", "raw-revoke-detail",
	} {
		require.NotContains(t, rawAudit, secret)
	}
}

func TestSettingsMutationIsBlockedWhenAuditAppendFails(t *testing.T) {
	upstream := newFakeSettingsUpstream(t)
	env := newManagerTestEnv(t)
	server := addFakeSettingsServer(t, env, upstream)

	auditDir := filepath.Join(t.TempDir(), "audit-is-a-directory")
	require.NoError(t, os.Mkdir(auditDir, 0o700))
	env.inventory.audit = NewAuditLog(auditDir)

	resp, body := env.putJSON(t, "/api/servers/"+server.ID+"/settings/mode", map[string]any{"mode": "tcp"})
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode, body)
	require.Contains(t, body, "audit log is unavailable")
	require.Zero(t, upstream.attempts("mode"), "a mutation must not reach upstream when the audit append fails")
	require.Zero(t, upstream.mutationCount())
}

func TestSettingsValidationRejectsBeforeUpstream(t *testing.T) {
	upstream := newFakeSettingsUpstream(t)
	env := newManagerTestEnv(t)
	server := addFakeSettingsServer(t, env, upstream)
	base := "/api/servers/" + server.ID + "/settings"

	cases := []struct {
		name    string
		path    string
		payload map[string]any
		code    string
	}{
		{"mode required", "/mode", map[string]any{"mode": ""}, "invalid_mode"},
		{"ports reserve", "/ports", map[string]any{
			"allow": []string{}, "deny": []string{}, "auto_range": "", "offline_reserve_seconds": 0,
		}, "invalid_ports"},
		{"web password mismatch", "/web", map[string]any{
			"username": "ops", "new_password": "long-enough-pw", "confirm_password": "different-pw",
		}, "password_mismatch"},
		{"rotate short auth", "/credentials", map[string]any{
			"auth": "short", "control_token": "long-enough-token-1234", "sync_mode": "auto",
		}, "invalid_credentials"},
		{"rotate sync mode", "/credentials", map[string]any{
			"auth": "long-enough-auth", "control_token": "long-enough-token-1234", "sync_mode": "later",
		}, "invalid_sync_mode"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resp, body := env.putJSON(t, base+testCase.path, testCase.payload)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode, body)
			require.Contains(t, body, testCase.code)
		})
	}
	require.Zero(t, upstream.mutationCount(), "invalid manager input must be rejected before any upstream mutation")
}

func TestSettingsPageRequiresAuthAndInventory(t *testing.T) {
	store := testStore(t)
	audit := testAudit(t)
	web, err := NewWebServer(store, audit, testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	ts := httptest.NewServer(web.Handler())
	t.Cleanup(ts.Close)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(ts.URL + "/servers/srv_missing/settings")
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, "unauthenticated settings page must redirect to login")

	login, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"},
		"password": {"pw"},
	})
	require.NoError(t, err)
	readBody(t, login)
	require.Equal(t, http.StatusNoContent, login.StatusCode)
	require.NotEmpty(t, login.Cookies())

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/servers/srv_missing/settings", nil)
	require.NoError(t, err)
	req.AddCookie(login.Cookies()[0])
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "a manager without inventory must report settings unavailable")
}

func (e *managerTestEnv) putForm(t *testing.T, path string, values url.Values, acceptHTML bool) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, e.server.URL+path, strings.NewReader(values.Encode()))
	require.NoError(t, err)
	req.AddCookie(e.cookie)
	req.Header.Set("X-Requested-With", "hy2tun")
	req.Header.Set("Origin", e.server.URL)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if acceptHTML {
		req.Header.Set("Accept", "text/html")
	}
	resp, err := e.server.Client().Do(req)
	require.NoError(t, err)
	return resp, readBody(t, resp)
}
