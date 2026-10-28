package serverui

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/credentials"
	"hy2tun/internal/testutil"
	"hy2tun/internal/view"
)

var testSessionKey = []byte("0123456789abcdef0123456789abcdef")

// fakeServerAPI is the in-memory server API used by the handler tests.
type fakeServerAPI struct {
	mu sync.Mutex

	overview view.Overview
	clients  []view.ClientView
	summary  ConfigSummary

	pushErr      error
	pushRevision int64

	pushes       int
	pushID       string
	pushBase     int64
	pushMappings []config.Mapping

	settings SettingsView

	updateWebCalls    int
	updateWebUsername string
	updateWebHash     string
	updateWebErr      error
	updateWebHook     func(username, passwordHash string)

	updatePortsCalls   int
	updatePortsAllow   []string
	updatePortsDeny    []string
	updatePortsAuto    string
	updatePortsReserve time.Duration
	updatePortsErr     error

	rotateCalls      int
	rotateAuth       string
	rotateToken      string
	rotateSync       string
	rotateGeneration int64
	rotateResults    []view.CredentialResult
	rotateErr        error

	revokeCalls int
	revokeErr   error

	snapshot *credentials.Set

	setTrustProxyCalls int
	setTrustProxyValue bool
	setTrustProxyErr   error
}

func (f *fakeServerAPI) Overview() view.Overview {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.overview
}

func (f *fakeServerAPI) Clients() []view.ClientView {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]view.ClientView, len(f.clients))
	copy(out, f.clients)
	return out
}

func (f *fakeServerAPI) Client(id string) (view.ClientView, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, client := range f.clients {
		if client.ID == id {
			return client, true
		}
	}
	return view.ClientView{}, false
}

func (f *fakeServerAPI) ConfigSummary() ConfigSummary {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.summary
}

func (f *fakeServerAPI) PushMappings(id string, baseRevision int64, mappings []config.Mapping) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pushes++
	f.pushID = id
	f.pushBase = baseRevision
	f.pushMappings = append([]config.Mapping(nil), mappings...)
	if f.pushErr != nil {
		return 0, f.pushErr
	}

	revision := f.pushRevision
	if revision == 0 {
		revision = baseRevision + 1
	}
	for i := range f.clients {
		if f.clients[i].ID != id {
			continue
		}
		f.clients[i].Revision = revision
		f.clients[i].Mappings = nil
		for _, mapping := range mappings {
			f.clients[i].Mappings = append(f.clients[i].Mappings, view.MappingView{
				Mapping:      mapping,
				Status:       "running",
				AssignedPort: mapping.RemotePort,
			})
		}
	}
	return revision, nil
}

func (f *fakeServerAPI) Settings() SettingsView {
	f.mu.Lock()
	defer f.mu.Unlock()

	settings := f.settings
	if settings.WebUsername == "" {
		settings.WebUsername = f.summary.WebUsername
	}
	if settings.Ports.Allow == nil {
		settings.Ports.Allow = append([]string(nil), f.summary.PortsAllow...)
	}
	if settings.Ports.Deny == nil {
		settings.Ports.Deny = append([]string(nil), f.summary.PortsDeny...)
	}
	if settings.Ports.AutoRange == "" {
		settings.Ports.AutoRange = f.summary.PortsAutoRange
	}
	if settings.Ports.OfflineReserveSeconds == 0 {
		settings.Ports.OfflineReserveSeconds = f.summary.OfflineReserveSeconds
		settings.Ports.OfflineReserve = f.summary.OfflineReserve
	}
	if settings.ReadOnly == nil && f.summary.ConfigPath != "" {
		settings.ReadOnly = []ReadOnlySetting{{Label: "配置文件", Value: f.summary.ConfigPath}}
	}
	return settings
}

func (f *fakeServerAPI) UpdateWebConfig(username, passwordHash string) error {
	f.mu.Lock()
	f.updateWebCalls++
	f.updateWebUsername = username
	f.updateWebHash = passwordHash
	err := f.updateWebErr
	hook := f.updateWebHook
	f.mu.Unlock()
	if err != nil {
		return err
	}
	// Production's server.UpdateWebConfig updates the live console
	// credentials. The hook lets handler tests model that without importing
	// the server package (which imports serverui).
	if hook != nil {
		hook(username, passwordHash)
	}
	return nil
}

func (f *fakeServerAPI) UpdatePorts(allow, deny []string, autoRange string, offlineReserve time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updatePortsCalls++
	f.updatePortsAllow = append([]string(nil), allow...)
	f.updatePortsDeny = append([]string(nil), deny...)
	f.updatePortsAuto = autoRange
	f.updatePortsReserve = offlineReserve
	return f.updatePortsErr
}

func (f *fakeServerAPI) RotateCredentials(auth, controlToken, syncMode string) (int64, []view.CredentialResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rotateCalls++
	f.rotateAuth = auth
	f.rotateToken = controlToken
	f.rotateSync = syncMode
	return f.rotateGeneration, append([]view.CredentialResult(nil), f.rotateResults...), f.rotateErr
}

func (f *fakeServerAPI) RevokePreviousCredentials() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeCalls++
	return f.revokeErr
}

func (f *fakeServerAPI) CredentialsSnapshot() *credentials.Set {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot.Clone()
}

func (f *fakeServerAPI) SetTrustProxy(trust bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setTrustProxyCalls++
	f.setTrustProxyValue = trust
	return f.setTrustProxyErr
}

// fakePushError mirrors server.PushError without importing the server package
// (which imports this package).
type fakePushError struct {
	results []control.Result
}

func (e *fakePushError) Error() string { return "mapping push rejected" }

func (e *fakePushError) MappingResults() []control.Result { return e.results }

func mustHash(password string) string {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	return string(hash)
}

func newTestWeb(t *testing.T, api API) (*Server, *httptest.Server) {
	t.Helper()

	web, err := NewServer(api, config.WebConfig{
		Listen:       "127.0.0.1:0",
		Username:     "admin",
		PasswordHash: mustHash("pw"),
	}, testSessionKey, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = web.Close() })

	ts := httptest.NewServer(web.Handler())
	t.Cleanup(ts.Close)
	return web, ts
}

func login(t *testing.T, ts *httptest.Server) *http.Cookie {
	t.Helper()

	resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"},
		"password": {"pw"},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NotEmpty(t, resp.Cookies())
	return resp.Cookies()[0]
}

func noRedirectClient(ts *httptest.Server) *http.Client {
	client := ts.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func doRequest(t *testing.T, client *http.Client, method, target, body string, cookie *http.Cookie, headers map[string]string) *http.Response {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, target, reader)
	require.NoError(t, err)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(data)
}

func readJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var value T
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&value))
	return value
}

func csrfHeaders(origin string) map[string]string {
	headers := map[string]string{
		"Content-Type":     "application/json",
		"X-Requested-With": "hy2tun",
	}
	if origin != "" {
		headers["Origin"] = origin
	}
	return headers
}

func TestServerWebLoginAndOverview(t *testing.T) {
	api := &fakeServerAPI{
		overview: view.Overview{
			OnlineClients:  1,
			ActiveMappings: 2,
			TxBytes:        1024,
			RxBytes:        2048,
			Listen:         "127.0.0.1:443",
			Uptime:         2 * time.Hour,
		},
		clients: []view.ClientView{{ID: "c1", Name: "home", Online: true}},
	}
	web, ts := newTestWeb(t, api)
	_ = web

	client := ts.Client()
	resp, err := client.PostForm(ts.URL+"/api/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NotEmpty(t, resp.Cookies())
	cookie := resp.Cookies()[0]
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/clients", nil)
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got []view.ClientView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	resp.Body.Close()
	require.Len(t, got, 1)

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/overview", nil)
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var overview overviewResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&overview))
	resp.Body.Close()
	require.Equal(t, 1, overview.OnlineClients)
	require.Equal(t, 2, overview.ActiveMappings)
	require.Equal(t, uint64(1024), overview.TxBytes)
	require.Equal(t, "127.0.0.1:443", overview.Listen)
	require.Equal(t, int64(7200), overview.UptimeSeconds)

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readBody(t, resp)
	require.Contains(t, body, "概览")
	require.Contains(t, body, "在线客户端")

	// Unauthenticated pages redirect, API calls get 401.
	resp = doRequest(t, noRedirectClient(ts), http.MethodGet, ts.URL+"/", "", nil, nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/login", resp.Header.Get("Location"))
	resp.Body.Close()

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/clients", "", nil, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// Wrong credentials are rejected.
	resp, err = client.PostForm(ts.URL+"/api/login", url.Values{"username": {"admin"}, "password": {"wrong"}})
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}

func TestServerWebLoginFormRedirects(t *testing.T) {
	_, ts := newTestWeb(t, &fakeServerAPI{})

	form := url.Values{"username": {"admin"}, "password": {"pw"}}.Encode()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/login", strings.NewReader(form))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := noRedirectClient(ts).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/", resp.Header.Get("Location"))
	require.NotEmpty(t, resp.Cookies())
}

func TestServerWebLoginJSON(t *testing.T) {
	_, ts := newTestWeb(t, &fakeServerAPI{})

	resp := doRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/login",
		`{"username":"admin","password":"pw"}`,
		nil, map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NotEmpty(t, resp.Cookies())
	resp.Body.Close()

	resp = doRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/login",
		`{"username":"admin","password":"wrong"}`,
		nil, map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	resp = doRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/login",
		`{`, nil, map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/login", "", nil, nil)
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	resp.Body.Close()
}

func TestServerWebLogout(t *testing.T) {
	_, ts := newTestWeb(t, &fakeServerAPI{})
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/logout", "", cookie, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	resp = doRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/logout", "", cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	setCookie := strings.Join(resp.Header.Values("Set-Cookie"), ";")
	require.Contains(t, setCookie, "hy2tun_session=")
	require.Contains(t, setCookie, "Max-Age=0")
	resp.Body.Close()

	// The captured cookie is revoked server-side, not just cleared in the
	// browser.
	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/clients", "", cookie, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// The cleared cookie no longer authenticates.
	cleared := &http.Cookie{Name: "hy2tun_session", Value: ""}
	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/clients", "", cleared, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}

func TestMutationsRequireCustomHeaderAndOrigin(t *testing.T) {
	api := &fakeServerAPI{
		clients: []view.ClientView{{ID: "c1", Name: "home", Online: true, Revision: 1}},
	}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	body := `{"base_revision":1,"mappings":[]}`
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/clients/c1/mappings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/clients/c1/mappings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "hy2tun")
	req.Header.Set("Origin", "http://evil.example")
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// A cross-site fetch metadata header is rejected even with a matching
	// Origin.
	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/clients/c1/mappings", body, cookie, map[string]string{
		"Content-Type":     "application/json",
		"X-Requested-With": "hy2tun",
		"Origin":           ts.URL,
		"Sec-Fetch-Site":   "cross-site",
	})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// The Origin scheme must match the request scheme, not just the host.
	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/clients/c1/mappings", body, cookie, map[string]string{
		"Content-Type":     "application/json",
		"X-Requested-With": "hy2tun",
		"Origin":           strings.Replace(ts.URL, "http://", "https://", 1),
	})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	require.Zero(t, api.pushes)
}

func TestLoginRateLimit(t *testing.T) {
	_, ts := newTestWeb(t, &fakeServerAPI{})

	client := ts.Client()
	for i := 0; i < 5; i++ {
		resp, err := client.PostForm(ts.URL+"/api/login", url.Values{"username": {"admin"}, "password": {"wrong"}})
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		resp.Body.Close()
	}
	resp, err := client.PostForm(ts.URL+"/api/login", url.Values{"username": {"admin"}, "password": {"wrong"}})
	require.NoError(t, err)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "60", resp.Header.Get("Retry-After"))
	resp.Body.Close()
}

func TestServerWebPushMappings(t *testing.T) {
	api := &fakeServerAPI{
		clients: []view.ClientView{{
			ID:       "c1",
			Name:     "home",
			Online:   true,
			Revision: 3,
			Mappings: []view.MappingView{{
				Mapping:      config.Mapping{ID: "m1", Type: "tcp", RemotePort: 2222, LocalAddr: "127.0.0.1:22", Enabled: true},
				Status:       "running",
				AssignedPort: 2222,
			}},
		}},
	}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	body := `{"base_revision":3,"mappings":[` +
		`{"id":"m1","type":"tcp","remote_port":2222,"local_addr":"127.0.0.1:22","note":"ssh","enabled":true},` +
		`{"id":"m2","type":"udp","remote_port":0,"local_addr":"127.0.0.1:51820","note":"wg","enabled":true}]}`
	resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/clients/c1/mappings", body, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var result struct {
		Revision int64 `json:"revision"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	resp.Body.Close()
	require.Equal(t, int64(4), result.Revision)

	require.Equal(t, 1, api.pushes)
	require.Equal(t, "c1", api.pushID)
	require.Equal(t, int64(3), api.pushBase)
	require.Len(t, api.pushMappings, 2)
	require.Equal(t, "ssh", api.pushMappings[0].Note)
	require.Equal(t, "wg", api.pushMappings[1].Note)
	require.Zero(t, api.pushMappings[1].RemotePort, "auto mappings must keep remote_port 0")

	// The detail fragment reflects the new revision and mappings.
	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/clients/c1/detail", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	detail := readBody(t, resp)
	require.Contains(t, detail, `data-revision="4"`)
	require.Contains(t, detail, "ssh")
	require.Contains(t, detail, "wg")

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/clients/c1", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	client := readJSON[view.ClientView](t, resp)
	require.Equal(t, int64(4), client.Revision)
}

func TestServerWebAutoMappingKeepsAutoPort(t *testing.T) {
	api := &fakeServerAPI{
		clients: []view.ClientView{{
			ID:       "c1",
			Name:     "home",
			Online:   true,
			Revision: 2,
			Mappings: []view.MappingView{{
				Mapping: config.Mapping{
					ID:         "m-auto",
					Type:       "udp",
					RemotePort: 0,
					LocalAddr:  "127.0.0.1:51820",
					Note:       "auto",
					Enabled:    true,
				},
				Status:       "running",
				AssignedPort: 20001,
			}},
		}},
	}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/clients/c1/detail", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readBody(t, resp)
	require.Contains(t, body, `data-mapping-id="m-auto"`)
	// The drawer's collectMappings reads data-mapping-port. Auto rows must
	// keep 0 so a note-only edit does not pin the server-assigned port.
	require.Contains(t, body, `data-mapping-port="0"`)
	require.Contains(t, body, `data-mapping-assigned="20001"`)
	require.Contains(t, body, "20001")

	// A note-only edit keeps remote_port 0, so the server keeps the
	// auto-assigned port instead of pinning it.
	noteOnly := `{"base_revision":2,"mappings":[{"id":"m-auto","type":"udp","remote_port":0,"local_addr":"127.0.0.1:51820","note":"renamed","enabled":true}]}`
	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/clients/c1/mappings", noteOnly, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, api.pushes)
	require.Zero(t, api.pushMappings[0].RemotePort)
	require.Equal(t, "renamed", api.pushMappings[0].Note)
}

func TestServerWebPushErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"conflict", control.ErrConflict, http.StatusConflict, "conflict"},
		{"offline", control.ErrClientOffline, http.StatusConflict, "client_offline"},
		{"not ready", control.ErrClientNotReady, http.StatusConflict, "client_not_ready"},
		{"timeout", control.ErrPushTimeout, http.StatusGatewayTimeout, "push_timeout"},
		{"bad mapping", &fakePushError{results: []control.Result{{ID: "m1", Error: "bad_message"}}}, http.StatusBadRequest, "bad_message"},
		{"port not allowed", &fakePushError{results: []control.Result{{ID: "m1", Error: "port_not_allowed"}}}, http.StatusConflict, "port_not_allowed"},
		{"port in use", &fakePushError{results: []control.Result{{ID: "m1", Error: "port_in_use", UDPToken: "secret-token"}}}, http.StatusConflict, "port_in_use"},
		{"internal", errors.New("boom"), http.StatusInternalServerError, "internal"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeServerAPI{
				clients: []view.ClientView{{ID: "c1", Online: true, Revision: 1}},
				pushErr: tc.err,
			}
			_, ts := newTestWeb(t, api)
			cookie := login(t, ts)

			body := `{"base_revision":1,"mappings":[{"id":"m1","type":"tcp","remote_port":2222,"local_addr":"127.0.0.1:22","enabled":true}]}`
			resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/clients/c1/mappings", body, cookie, csrfHeaders(ts.URL))
			require.Equal(t, tc.status, resp.StatusCode)
			raw := readBody(t, resp)
			require.NotContains(t, raw, "secret-token")
			var payload struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			require.NoError(t, json.Unmarshal([]byte(raw), &payload))
			require.Equal(t, tc.code, payload.Code)
			require.NotEmpty(t, payload.Error)
		})
	}
}

func TestServerWebPushValidation(t *testing.T) {
	note := strings.Repeat("a", 129)
	cases := []struct {
		name string
		body string
	}{
		{"empty id", `{"base_revision":1,"mappings":[{"id":"","type":"tcp","remote_port":2222,"local_addr":"127.0.0.1:22","enabled":true}]}`},
		{"duplicate id", `{"base_revision":1,"mappings":[{"id":"m1","type":"tcp","remote_port":2222,"local_addr":"127.0.0.1:22","enabled":true},{"id":"m1","type":"udp","remote_port":2223,"local_addr":"127.0.0.1:23","enabled":true}]}`},
		{"bad type", `{"base_revision":1,"mappings":[{"id":"m1","type":"http","remote_port":2222,"local_addr":"127.0.0.1:22","enabled":true}]}`},
		{"empty local addr", `{"base_revision":1,"mappings":[{"id":"m1","type":"tcp","remote_port":2222,"local_addr":"","enabled":true}]}`},
		{"local addr without port", `{"base_revision":1,"mappings":[{"id":"m1","type":"tcp","remote_port":2222,"local_addr":"127.0.0.1","enabled":true}]}`},
		{"local addr without host", `{"base_revision":1,"mappings":[{"id":"m1","type":"tcp","remote_port":2222,"local_addr":":22","enabled":true}]}`},
		{"port out of range", `{"base_revision":1,"mappings":[{"id":"m1","type":"tcp","remote_port":70000,"local_addr":"127.0.0.1:22","enabled":true}]}`},
		{"note too long", `{"base_revision":1,"mappings":[{"id":"m1","type":"tcp","remote_port":2222,"local_addr":"127.0.0.1:22","note":"` + note + `","enabled":true}]}`},
		{"negative revision", `{"base_revision":-1,"mappings":[]}`},
		{"malformed", `{`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeServerAPI{clients: []view.ClientView{{ID: "c1", Online: true, Revision: 1}}}
			_, ts := newTestWeb(t, api)
			cookie := login(t, ts)

			resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/clients/c1/mappings", tc.body, cookie, csrfHeaders(ts.URL))
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			resp.Body.Close()
			require.Zero(t, api.pushes)
		})
	}
}

func TestServerWebPushRequiresOrigin(t *testing.T) {
	api := &fakeServerAPI{clients: []view.ClientView{{ID: "c1", Online: true, Revision: 1}}}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	body := `{"base_revision":1,"mappings":[]}`
	resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/clients/c1/mappings", body, cookie, map[string]string{
		"Content-Type":     "application/json",
		"X-Requested-With": "hy2tun",
	})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
	require.Zero(t, api.pushes)
}

func TestServerWebClientsPages(t *testing.T) {
	now := time.Now()
	api := &fakeServerAPI{
		overview: view.Overview{OnlineClients: 1, ActiveMappings: 1, Listen: "0.0.0.0:443", Uptime: time.Hour},
		clients: []view.ClientView{
			{
				ID:       "c1",
				Name:     "home",
				Version:  "v1",
				Revision: 3,
				Online:   true,
				LastSeen: now,
				Mappings: []view.MappingView{{
					Mapping: config.Mapping{
						ID:         "m1",
						Type:       "tcp",
						RemotePort: 2222,
						LocalAddr:  "127.0.0.1:22",
						Note:       `<script>alert(1)</script>`,
						Enabled:    true,
					},
					Status:       "running",
					AssignedPort: 2222,
					TxBytes:      1024,
					RxBytes:      2048,
				}},
			},
			{
				ID:       "c2",
				Name:     "office",
				Revision: 1,
				Online:   false,
				LastSeen: now.Add(-time.Hour),
				Mappings: []view.MappingView{{
					Mapping:      config.Mapping{ID: "m2", Type: "udp", RemotePort: 5353, LocalAddr: "127.0.0.1:53", Enabled: true},
					Status:       "offline",
					AssignedPort: 5353,
				}},
			},
		},
		summary: ConfigSummary{
			ConfigPath:             "/etc/hy2tun/server.yaml",
			Listen:                 "0.0.0.0:443",
			ListenHost:             "0.0.0.0",
			WebListen:              "127.0.0.1:7500",
			WebUsername:            "admin",
			StateFile:              "/var/lib/hy2tun/state.json",
			PortsAllow:             []string{"2222", "8000-8100"},
			PortsAutoRange:         "20000-29999",
			OfflineReserve:         24 * time.Hour,
			OfflineReserveSeconds:  86400,
			LogLevel:               "info",
			Version:                "dev",
			ControlTokenConfigured: true,
			WebPasswordConfigured:  true,
		},
	}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/clients", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readBody(t, resp)
	require.Contains(t, body, "home")
	require.Contains(t, body, "office")
	require.Contains(t, body, "选择一个客户端")

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/clients?id=c1", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body = readBody(t, resp)
	require.Contains(t, body, "has-selection")
	require.Contains(t, body, "编辑")
	require.Contains(t, body, "127.0.0.1:22")
	require.Contains(t, body, `id="mapping-drawer"`)
	require.Contains(t, body, `id="mapping-form"`)
	require.Contains(t, body, `data-mapping-id="m1"`)
	require.NotContains(t, body, `<script>alert(1)</script>`)
	require.Contains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;")

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/clients?id=missing", "", cookie, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Contains(t, readBody(t, resp), "不存在")

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/clients/c1/detail", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	detail := readBody(t, resp)
	require.Contains(t, detail, `data-client-id="c1"`)
	require.Contains(t, detail, `data-revision="3"`)
	require.Contains(t, detail, `data-mapping-id="m1"`)
	require.Contains(t, detail, `data-mapping-local="127.0.0.1:22"`)
	require.Contains(t, detail, "&lt;script&gt;")
	require.Contains(t, detail, "新增映射")

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/clients/c2/detail", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, readBody(t, resp), "只读")

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/clients/missing/detail", "", cookie, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/mappings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body = readBody(t, resp)
	require.Contains(t, body, "全部映射")
	require.Contains(t, body, "home")
	require.Contains(t, body, "office")
	require.Contains(t, body, `data-filter="type"`)
	require.Contains(t, body, `data-filter="status"`)
	require.Contains(t, body, `data-filter="port"`)

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/settings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body = readBody(t, resp)
	require.Contains(t, body, "/etc/hy2tun/server.yaml")
	require.Contains(t, body, "端口策略")
	require.Contains(t, body, "1天 0小时")

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/config/summary", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	summary := readJSON[ConfigSummary](t, resp)
	require.Equal(t, "/etc/hy2tun/server.yaml", summary.ConfigPath)

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/mappings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	entries := readJSON[[]mappingEntry](t, resp)
	require.Len(t, entries, 2)
	require.Equal(t, "c1", entries[0].ClientID)

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/clients/c1", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	client := readJSON[view.ClientView](t, resp)
	require.Equal(t, int64(3), client.Revision)

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/clients/missing", "", cookie, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()

	// The htmx fragment endpoint rejects an expired session with a redirect
	// header instead of swapping the login page into the detail pane.
	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/clients/c1/detail", "", nil, map[string]string{"HX-Request": "true"})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t, "/login", resp.Header.Get("HX-Redirect"))
	resp.Body.Close()
}

func TestSetTrustProxyUpdatesLoginHandler(t *testing.T) {
	web, _ := newTestWeb(t, &fakeServerAPI{})

	require.False(t, web.login.TrustProxy)
	web.SetTrustProxy(true)
	require.True(t, web.login.TrustProxy)
	web.SetTrustProxy(false)
	require.False(t, web.login.TrustProxy)
}

func TestServerWebStaticAndLoginPage(t *testing.T) {
	_, ts := newTestWeb(t, &fakeServerAPI{})

	resp := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/static/app.css", "", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/css")
	require.Contains(t, readBody(t, resp), "--bg")

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/static/app.js", "", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, readBody(t, resp), "EventSource")

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/static/htmx.min.js", "", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "javascript")
	require.Greater(t, len(readBody(t, resp)), 10000)

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/login", "", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	require.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	require.Contains(t, resp.Header.Get("Content-Security-Policy"), "default-src 'self'")
	body := readBody(t, resp)
	require.Contains(t, body, `action="/api/login"`)
	require.Contains(t, body, "登录管理控制台")

	cookie := login(t, ts)
	resp = doRequest(t, noRedirectClient(ts), http.MethodGet, ts.URL+"/login", "", cookie, nil)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/", resp.Header.Get("Location"))
	resp.Body.Close()
}

func TestServerWebHSTSBehindTLSProxy(t *testing.T) {
	web, _ := newTestWeb(t, &fakeServerAPI{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/login", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	recorder := httptest.NewRecorder()
	web.Handler().ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "max-age=31536000", recorder.Header().Get("Strict-Transport-Security"))

	// Without the forwarded scheme, HSTS must not be sent over plain HTTP.
	req = httptest.NewRequest(http.MethodGet, "http://example.com/login", nil)
	recorder = httptest.NewRecorder()
	web.Handler().ServeHTTP(recorder, req)
	require.Empty(t, recorder.Header().Get("Strict-Transport-Security"))
}

func TestServerWebSSE(t *testing.T) {
	api := &fakeServerAPI{
		overview: view.Overview{OnlineClients: 1, ActiveMappings: 2, TxBytes: 10, RxBytes: 20, Listen: "127.0.0.1:443", Uptime: time.Hour},
		clients:  []view.ClientView{{ID: "c1", Name: "home", Online: true, Revision: 2, Mappings: []view.MappingView{{Status: "running"}}}},
	}
	web, ts := newTestWeb(t, api)
	web.eventInterval = 20 * time.Millisecond
	cookie := login(t, ts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
	require.Contains(t, resp.Header.Get("Cache-Control"), "no-cache")

	reader := bufio.NewReader(resp.Body)
	event, data := readSSEEvent(t, reader)
	require.Equal(t, "snapshot", event)
	var snapshot sseSnapshot
	require.NoError(t, json.Unmarshal([]byte(data), &snapshot))
	require.Equal(t, 1, snapshot.Overview.OnlineClients)
	require.Len(t, snapshot.Clients, 1)
	require.Equal(t, "c1", snapshot.Clients[0].ID)

	event, data = readSSEEvent(t, reader)
	require.Equal(t, "snapshot", event)
	require.NotEmpty(t, data)

	cancel()

	// Unauthenticated SSE is rejected.
	resp2 := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/events", "", nil, nil)
	require.Equal(t, http.StatusUnauthorized, resp2.StatusCode)
	resp2.Body.Close()
}

func readSSEEvent(t *testing.T, reader *bufio.Reader) (string, string) {
	t.Helper()

	for {
		var event, data string
		for {
			line, err := reader.ReadString('\n')
			require.NoError(t, err)
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		// The stream starts with a retry hint and may contain keepalive
		// comments; skip blocks that carry no event or data.
		if event != "" || data != "" {
			return event, data
		}
	}
}

func TestServerWebStartAndClose(t *testing.T) {
	web, err := NewServer(&fakeServerAPI{}, config.WebConfig{
		Listen:       "127.0.0.1:0",
		Username:     "admin",
		PasswordHash: mustHash("pw"),
	}, testSessionKey, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = web.Close() })

	require.NoError(t, web.Start())
	require.Error(t, web.Start())

	addr := web.Addr()
	require.NotEmpty(t, addr)
	require.NotEqual(t, "127.0.0.1:0", addr)

	resp, err := http.Get("http://" + addr + "/login")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	require.NoError(t, web.Close())
	require.NoError(t, web.Close())
	require.Error(t, web.Start())
}

func TestServerWebStartTLS(t *testing.T) {
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)

	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	require.NoError(t, os.WriteFile(certPath, certPEM, 0600))
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0600))

	web, err := NewServer(&fakeServerAPI{}, config.WebConfig{
		Listen:       "127.0.0.1:0",
		Username:     "admin",
		PasswordHash: mustHash("pw"),
		TLS:          config.WebTLSConfig{Cert: certPath, Key: keyPath},
	}, testSessionKey, slog.Default())
	require.NoError(t, err)
	require.NoError(t, web.Start())
	t.Cleanup(func() { _ = web.Close() })

	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	resp, err := client.Get("https://" + web.Addr() + "/login")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotNil(t, resp.TLS)
	resp.Body.Close()
}

func TestServerWebNewValidation(t *testing.T) {
	cfg := config.WebConfig{Listen: "127.0.0.1:0", Username: "admin", PasswordHash: mustHash("pw")}

	_, err := NewServer(nil, cfg, testSessionKey, slog.Default())
	require.Error(t, err)

	_, err = NewServer(&fakeServerAPI{}, cfg, []byte("short"), slog.Default())
	require.Error(t, err)

	emptyHash := cfg
	emptyHash.PasswordHash = ""
	_, err = NewServer(&fakeServerAPI{}, emptyHash, testSessionKey, slog.Default())
	require.Error(t, err)

	badHash := cfg
	badHash.PasswordHash = "not-a-bcrypt-hash"
	_, err = NewServer(&fakeServerAPI{}, badHash, testSessionKey, slog.Default())
	require.Error(t, err)

	badTLS := cfg
	badTLS.TLS = config.WebTLSConfig{Cert: "cert.pem"}
	_, err = NewServer(&fakeServerAPI{}, badTLS, testSessionKey, slog.Default())
	require.Error(t, err)
}

func TestServerSettingsAPIReturnsMaskedValues(t *testing.T) {
	api := &fakeServerAPI{
		settings: SettingsView{
			WebUsername:           "admin",
			WebPasswordConfigured: true,
			AuthType:              "password",
			TrustProxy:            true,
			Ports: PortSettingsView{
				Allow:                 []string{"2222", "8000-8100"},
				Deny:                  []string{"9000"},
				AutoRange:             "20000-29999",
				OfflineReserveSeconds: 3600,
				OfflineReserve:        time.Hour,
			},
			Credentials: CredentialSettingsView{
				Generation:                 2,
				PreviousGeneration:         1,
				CurrentAuthMasked:          MaskedSecret,
				CurrentControlTokenMasked:  MaskedSecret,
				PreviousAuthMasked:         MaskedSecret,
				PreviousControlTokenMasked: MaskedSecret,
				Rotatable:                  true,
			},
			Clients: []ClientCredentialView{
				{ClientID: "c1", Name: "home", Online: true, Generation: 2, Status: "synced"},
				{ClientID: "c2", Name: "office", Online: false, Generation: 1, Status: "pending"},
			},
			ReadOnly: []ReadOnlySetting{{Label: "HY2 监听", Value: "0.0.0.0:443"}},
		},
	}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/settings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	raw := readBody(t, resp)
	require.NotContains(t, raw, "current_auth\"")
	require.NotContains(t, raw, "control_token\"")

	var settings SettingsView
	require.NoError(t, json.Unmarshal([]byte(raw), &settings))
	require.Equal(t, "admin", settings.WebUsername)
	require.Equal(t, MaskedSecret, settings.Credentials.CurrentAuthMasked)
	require.Equal(t, MaskedSecret, settings.Credentials.PreviousControlTokenMasked)
	require.Equal(t, int64(2), settings.Credentials.Generation)
	require.True(t, settings.TrustProxy)
	require.Len(t, settings.Clients, 2)
	require.Equal(t, "pending", settings.Clients[1].Status)

	// The rendered settings page carries the editable sections and never the
	// actual secrets.
	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/settings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	page := readBody(t, resp)
	for _, section := range []string{"Web 管理员", "HY2 凭据", "端口白名单", "代理信任", "只读项"} {
		require.Contains(t, page, section)
	}
	require.Contains(t, page, MaskedSecret)
	require.Contains(t, page, "home")
	require.Contains(t, page, "office")
}

func elementTag(t *testing.T, page, marker string) string {
	t.Helper()
	idx := strings.Index(page, marker)
	require.NotEqual(t, -1, idx, "marker %q not found", marker)
	start := strings.LastIndex(page[:idx], "<")
	require.NotEqual(t, -1, start)
	end := strings.Index(page[idx:], ">")
	require.NotEqual(t, -1, end)
	return page[start : idx+end+1]
}

func TestServerSettingsWarnsOnSecondRotation(t *testing.T) {
	t.Run("with previous generation", func(t *testing.T) {
		api := &fakeServerAPI{
			settings: SettingsView{
				Credentials: CredentialSettingsView{
					Generation:         2,
					PreviousGeneration: 1,
					Rotatable:          true,
				},
			},
		}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		// The settings JSON must expose the previous generation so the page
		// can decide whether a second rotation replaces it.
		resp := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/settings", "", cookie, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var settings SettingsView
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&settings))
		resp.Body.Close()
		require.Equal(t, int64(1), settings.Credentials.PreviousGeneration)

		resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/settings", "", cookie, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		page := readBody(t, resp)
		warningTag := elementTag(t, page, "data-second-rotation-warning")
		require.NotContains(t, warningTag, "hidden")
		require.Contains(t, page, "更早代际")
		require.Contains(t, page, "立即断开")
	})

	t.Run("without previous generation", func(t *testing.T) {
		api := &fakeServerAPI{
			settings: SettingsView{
				Credentials: CredentialSettingsView{
					Generation: 1,
					Rotatable:  true,
				},
			},
		}
		_, ts := newTestWeb(t, api)
		cookie := login(t, ts)

		resp := doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/settings", "", cookie, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		page := readBody(t, resp)
		warningTag := elementTag(t, page, "data-second-rotation-warning")
		require.Contains(t, warningTag, "hidden")
	})
}

func TestServerSettingsRotateCredentials(t *testing.T) {
	api := &fakeServerAPI{
		rotateGeneration: 3,
		rotateResults: []view.CredentialResult{
			{ClientID: "c1", Name: "home", Generation: 3, Status: "synced"},
			{ClientID: "c2", Name: "office", Generation: 3, Status: "pending", Error: "not_ready"},
		},
	}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	body := `{"current_password":"pw","auth":"new-auth-password","control_token":"0123456789abcdef","sync_mode":"auto"}`
	resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/credentials", body, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var result struct {
		Generation int64                   `json:"generation"`
		Results    []view.CredentialResult `json:"results"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	resp.Body.Close()
	require.Equal(t, int64(3), result.Generation)
	require.Len(t, result.Results, 2)
	require.Equal(t, "synced", result.Results[0].Status)

	require.Equal(t, 1, api.rotateCalls)
	require.Equal(t, "new-auth-password", api.rotateAuth)
	require.Equal(t, "0123456789abcdef", api.rotateToken)
	require.Equal(t, "auto", api.rotateSync)

	// A wrong current password never reaches the API.
	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/credentials",
		`{"current_password":"wrong","auth":"another-password","sync_mode":"manual"}`, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, api.rotateCalls)

	// The CSRF header and same-origin Origin remain mandatory.
	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/credentials", body, cookie, map[string]string{
		"Content-Type":     "application/json",
		"X-Requested-With": "hy2tun",
	})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, api.rotateCalls)
}

func TestServerSettingsRevealRequiresPasswordAndNoStore(t *testing.T) {
	api := &fakeServerAPI{snapshot: &credentials.Set{
		Generation:           2,
		Auth:                 "current-auth-secret",
		ControlToken:         "current-token-secret",
		PreviousGeneration:   1,
		PreviousAuth:         "previous-auth-secret",
		PreviousControlToken: "previous-token-secret",
	}}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/settings/credentials/reveal",
		`{"password":"wrong"}`, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	raw := readBody(t, resp)
	require.NotContains(t, raw, "current-auth-secret")
	require.NotContains(t, raw, "previous-token-secret")

	resp = doRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/settings/credentials/reveal",
		`{"password":"pw"}`, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var revealed struct {
		Current struct {
			Generation   int64  `json:"generation"`
			Auth         string `json:"auth"`
			ControlToken string `json:"control_token"`
		} `json:"current"`
		Previous struct {
			Generation   int64  `json:"generation"`
			Auth         string `json:"auth"`
			ControlToken string `json:"control_token"`
		} `json:"previous"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&revealed))
	resp.Body.Close()
	require.Equal(t, int64(2), revealed.Current.Generation)
	require.Equal(t, "current-auth-secret", revealed.Current.Auth)
	require.Equal(t, "current-token-secret", revealed.Current.ControlToken)
	require.Equal(t, int64(1), revealed.Previous.Generation)
	require.Equal(t, "previous-auth-secret", revealed.Previous.Auth)
	require.Equal(t, "previous-token-secret", revealed.Previous.ControlToken)

	// Missing Origin is rejected before the password is checked.
	resp = doRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/settings/credentials/reveal",
		`{"password":"pw"}`, cookie, map[string]string{
			"Content-Type":     "application/json",
			"X-Requested-With": "hy2tun",
		})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
}

func TestServerSettingsWebUpdateRevokesSession(t *testing.T) {
	api := &fakeServerAPI{}
	web, ts := newTestWeb(t, api)
	api.mu.Lock()
	api.updateWebHook = func(username, passwordHash string) {
		web.SetAdmin(username, passwordHash)
	}
	api.mu.Unlock()
	cookie := login(t, ts)

	body := `{"username":"root","current_password":"pw","new_password":"new-password"}`
	resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/web", body, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()

	require.Equal(t, 1, api.updateWebCalls)
	require.Equal(t, "root", api.updateWebUsername)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(api.updateWebHash), []byte("new-password")))

	// The old session cookie is revoked.
	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/settings", "", cookie, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// The new credentials work and the old ones do not.
	resp = doRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/login",
		`{"username":"admin","password":"pw"}`, nil, map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
	resp = doRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/login",
		`{"username":"root","password":"new-password"}`, nil, map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NotEmpty(t, resp.Cookies())
	newCookie := resp.Cookies()[0]
	resp.Body.Close()

	// A wrong current password is rejected without calling the API, even with
	// a valid session and CSRF headers.
	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/web",
		`{"username":"other","current_password":"wrong","new_password":"another-password"}`, newCookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, api.updateWebCalls)
}

func TestServerSettingsPortsAndTrustProxy(t *testing.T) {
	api := &fakeServerAPI{}
	web, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	body := `{"current_password":"pw","allow":["2222","8000-8100"],"deny":["9000"],"auto_range":"20000-29999","offline_reserve_seconds":7200}`
	resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/ports", body, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, api.updatePortsCalls)
	require.Equal(t, []string{"2222", "8000-8100"}, api.updatePortsAllow)
	require.Equal(t, []string{"9000"}, api.updatePortsDeny)
	require.Equal(t, "20000-29999", api.updatePortsAuto)
	require.Equal(t, 2*time.Hour, api.updatePortsReserve)

	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/ports",
		`{"current_password":"wrong","allow":[],"deny":[],"auto_range":"20000-29999","offline_reserve_seconds":60}`, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, api.updatePortsCalls)

	// The trust-proxy toggle has its own authenticated mutation and updates
	// the live login limiter.
	require.False(t, web.login.TrustProxy)
	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/proxy",
		`{"current_password":"pw","trust_proxy":true}`, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, api.setTrustProxyCalls)
	require.True(t, api.setTrustProxyValue)
	require.True(t, web.login.TrustProxy)

	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/proxy",
		`{"current_password":"pw","trust_proxy":false}`, cookie, map[string]string{
			"Content-Type":     "application/json",
			"X-Requested-With": "hy2tun",
		})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, api.setTrustProxyCalls)
}

func TestServerSettingsRevokePreviousCredentials(t *testing.T) {
	api := &fakeServerAPI{}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/settings/credentials/revoke",
		`{"current_password":"pw"}`, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, api.revokeCalls)

	resp = doRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/settings/credentials/revoke",
		`{"current_password":"wrong"}`, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, api.revokeCalls)
}

func TestServerSettingsWebUpdateFailureKeepsSession(t *testing.T) {
	api := &fakeServerAPI{updateWebErr: errors.New("save server config: write failed")}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/web",
		`{"username":"root","current_password":"pw","new_password":"new-password"}`, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, api.updateWebCalls)

	// A failed config write must not revoke the current session or change the
	// live login credentials.
	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/settings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp = doRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/login",
		`{"username":"admin","password":"pw"}`, nil, map[string]string{"Content-Type": "application/json"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()
}

func TestServerSettingsReauthRateLimitsMutations(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		wrongBody   string
		correctBody string
		apiCalls    func(*fakeServerAPI) int
	}{
		{
			name:        "web admin",
			path:        "/api/settings/web",
			wrongBody:   `{"username":"root","current_password":"wrong","new_password":"new-password"}`,
			correctBody: `{"username":"root","current_password":"pw","new_password":"new-password"}`,
			apiCalls:    func(api *fakeServerAPI) int { return api.updateWebCalls },
		},
		{
			name:        "rotate credentials",
			path:        "/api/settings/credentials",
			wrongBody:   `{"current_password":"wrong","auth":"new-auth-password","sync_mode":"manual"}`,
			correctBody: `{"current_password":"pw","auth":"new-auth-password","sync_mode":"manual"}`,
			apiCalls:    func(api *fakeServerAPI) int { return api.rotateCalls },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeServerAPI{}
			_, ts := newTestWeb(t, api)
			cookie := login(t, ts)

			for attempt := 1; attempt <= 5; attempt++ {
				resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+tc.path, tc.wrongBody, cookie, csrfHeaders(ts.URL))
				require.Equal(t, http.StatusForbidden, resp.StatusCode, "attempt %d", attempt)
				resp.Body.Close()
			}

			resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+tc.path, tc.wrongBody, cookie, csrfHeaders(ts.URL))
			require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
			require.Equal(t, "60", resp.Header.Get("Retry-After"))
			resp.Body.Close()

			// Even a correct password is rejected while the IP is locked.
			resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+tc.path, tc.correctBody, cookie, csrfHeaders(ts.URL))
			require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
			resp.Body.Close()

			require.Zero(t, tc.apiCalls(api), "a wrong or rate-limited confirmation must not reach the API")
		})
	}
}

func TestServerSettingsReauthCorrectPasswordBeforeLimit(t *testing.T) {
	api := &fakeServerAPI{}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	correct := `{"current_password":"pw","allow":[],"deny":[],"auto_range":"20000-29999","offline_reserve_seconds":60}`
	resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/ports", correct, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, api.updatePortsCalls)

	wrong := `{"username":"root","current_password":"wrong","new_password":"new-password"}`
	for attempt := 1; attempt <= 5; attempt++ {
		resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/web", wrong, cookie, csrfHeaders(ts.URL))
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "attempt %d", attempt)
		resp.Body.Close()
	}
	resp = doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/web", wrong, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	resp.Body.Close()
	require.Zero(t, api.updateWebCalls)
}

func TestServerSettingsWebUpdateDefersSessionUpdateToAPI(t *testing.T) {
	// The fake does not implement the server's live session update. A
	// successful handler call must therefore leave the old session intact:
	// exactly one layer (server.UpdateWebConfig in production) owns session
	// invalidation.
	api := &fakeServerAPI{}
	_, ts := newTestWeb(t, api)
	cookie := login(t, ts)

	body := `{"username":"root","current_password":"pw","new_password":"new-password"}`
	resp := doRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/web", body, cookie, csrfHeaders(ts.URL))
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, 1, api.updateWebCalls)

	resp = doRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/settings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}
