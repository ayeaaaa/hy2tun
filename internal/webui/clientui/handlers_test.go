package clientui

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
	"hy2tun/internal/credentials"
	"hy2tun/internal/testutil"
	"hy2tun/internal/view"
)

var testSessionKey = []byte("0123456789abcdef0123456789abcdef")

// fakeClientAPI is the in-memory client API used by the handler tests.
type fakeClientAPI struct {
	mu sync.Mutex

	status   view.ClientStatus
	mappings []view.MappingView
	logs     []view.LogEntry
	settings SettingsView

	rotation credentials.Set
	name     string
	web      fakeWebConfig

	addErr      error
	updateErr   error
	deleteErr   error
	proxyErr    error
	settingsErr error
	nameErr     error
	webErr      error
	rotateErr   error

	added   []config.Mapping
	updated []config.Mapping
	deleted []string

	lastLevel string
	lastLimit int
}

type fakeMultiAPI struct {
	fakeClientAPI
	servers map[string]*fakeClientAPI
	order   []string
}

func (f *fakeMultiAPI) Servers() []ServerSummary {
	out := make([]ServerSummary, 0, len(f.order))
	for _, name := range f.order {
		api := f.servers[name]
		status := api.Status()
		out = append(out, ServerSummary{
			Name:         name,
			Address:      status.Server,
			Online:       status.Connected,
			Mappings:     len(api.Mappings()),
			ProxyEnabled: api.settings.Proxy.Enabled,
		})
	}
	return out
}

func (f *fakeMultiAPI) Server(name string) (API, bool) {
	api, ok := f.servers[name]
	if !ok {
		return nil, false
	}
	return api, true
}

func (f *fakeMultiAPI) UpdateProxy(name string, cfg config.ProxyConfig) error {
	api, ok := f.servers[name]
	if !ok {
		return errors.New("server not found")
	}
	if api.proxyErr != nil {
		return api.proxyErr
	}
	api.settings.Proxy = ProxyView{Enabled: cfg.Enabled, SOCKS5: cfg.SOCKS5, HTTP: cfg.HTTP}
	return nil
}

func (f *fakeClientAPI) Status() view.ClientStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeClientAPI) Mappings() []view.MappingView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]view.MappingView(nil), f.mappings...)
}

func (f *fakeClientAPI) AddMapping(mapping config.Mapping) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, mapping)
	f.mappings = append(f.mappings, view.MappingView{Mapping: mapping, Status: "pending"})
	return nil
}

func (f *fakeClientAPI) UpdateMapping(mapping config.Mapping) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updated = append(f.updated, mapping)
	return nil
}

func (f *fakeClientAPI) DeleteMapping(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeClientAPI) Logs(level string, limit int) []view.LogEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastLevel = level
	f.lastLimit = limit

	out := make([]view.LogEntry, 0, len(f.logs))
	for _, entry := range f.logs {
		if level != "" && level != "all" && testLogRank(entry.Level) < testLogRank(level) {
			continue
		}
		out = append(out, entry)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

type fakeWebConfig struct {
	Username     string
	PasswordHash string
}

func (f *fakeClientAPI) Proxy() ProxyView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings.Proxy
}

func (f *fakeClientAPI) UpdateProxy(cfg config.ProxyConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.proxyErr != nil {
		return f.proxyErr
	}
	f.settings.Proxy = ProxyView{Enabled: cfg.Enabled, SOCKS5: cfg.SOCKS5, HTTP: cfg.HTTP}
	return nil
}

func (f *fakeClientAPI) Settings() SettingsView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings
}

func (f *fakeClientAPI) UpdateWebConfig(username, passwordHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.webErr != nil {
		return f.webErr
	}
	f.web = fakeWebConfig{Username: username, PasswordHash: passwordHash}
	f.settings.WebUsername = username
	f.settings.WebPasswordConfigured = passwordHash != ""
	return nil
}

func (f *fakeClientAPI) UpdateName(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nameErr != nil {
		return f.nameErr
	}
	f.name = name
	f.settings.Name = name
	return nil
}

func (f *fakeClientAPI) RotateLocalCredentials(generation int64, auth, controlToken string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rotateErr != nil {
		return f.rotateErr
	}
	f.rotation = credentials.Set{
		Generation:   generation,
		Auth:         auth,
		ControlToken: controlToken,
	}
	f.settings.Credentials.Generation = generation
	f.settings.Credentials.CurrentAuthMasked = MaskedSecret
	f.settings.Credentials.CurrentControlTokenMasked = MaskedSecret
	f.settings.Credentials.Synced = false
	return nil
}

func (f *fakeClientAPI) CredentialsSnapshot() *credentials.Set {
	f.mu.Lock()
	defer f.mu.Unlock()
	snapshot := f.rotation
	return &snapshot
}

func testLogRank(level string) int {
	switch strings.ToLower(level) {
	case "debug":
		return 1
	case "info":
		return 2
	case "warn":
		return 3
	case "error":
		return 4
	default:
		return 0
	}
}

func mustHash(password string) string {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		panic(err)
	}
	return string(hash)
}

func newTestClientWeb(t *testing.T, api API) (*Server, *httptest.Server) {
	t.Helper()

	web, err := NewClient(api, config.WebConfig{
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

func loginClientWeb(t *testing.T, ts *httptest.Server) *http.Cookie {
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

func doClientRequest(t *testing.T, client *http.Client, method, target, body string, cookie *http.Cookie, headers map[string]string) *http.Response {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, target, reader)
	require.NoError(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
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

func readClientBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}

func readClientJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var value T
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&value))
	return value
}

func TestClientWebAddMappingWritesConfig(t *testing.T) {
	api := &fakeClientAPI{}
	_, ts := newTestClientWeb(t, api)

	client := ts.Client()
	resp, err := client.PostForm(ts.URL+"/api/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	body := `{"type":"tcp","remote_port":0,"local_addr":"192.168.1.10:22","note":"NAS SSH","enabled":true}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/mappings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "hy2tun")
	req.Header.Set("Origin", ts.URL)
	req.AddCookie(resp.Cookies()[0])
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Len(t, api.added, 1)
	require.Equal(t, "NAS SSH", api.added[0].Note)
	require.NotEmpty(t, api.added[0].ID)

	created := readClientJSON[mappingResponse](t, resp)
	require.Equal(t, api.added[0].ID, created.ID)
	require.Equal(t, "NAS SSH", created.Note)
}

func TestClientWebUpdateMapping(t *testing.T) {
	api := &fakeClientAPI{}
	_, ts := newTestClientWeb(t, api)

	cookie := loginClientWeb(t, ts)
	body := `{"id":"m1","type":"tcp","remote_port":2222,"local_addr":"192.168.1.10:22","note":"changed","enabled":true}`
	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/mappings/m1", body, cookie, map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Len(t, api.updated, 1)
	require.Equal(t, "changed", api.updated[0].Note)
	require.Equal(t, "m1", api.updated[0].ID)
	readClientBody(t, resp)
}

func TestClientWebDeleteMapping(t *testing.T) {
	api := &fakeClientAPI{}
	_, ts := newTestClientWeb(t, api)

	cookie := loginClientWeb(t, ts)
	resp := doClientRequest(t, ts.Client(), http.MethodDelete, ts.URL+"/api/mappings/m1", "", cookie, map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Equal(t, []string{"m1"}, api.deleted)
	resp.Body.Close()
}

func TestClientWebMappingErrorStatusCodes(t *testing.T) {
	api := &fakeClientAPI{addErr: config.ErrMappingExists, updateErr: config.ErrMappingNotFound}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	body := `{"id":"m1","type":"tcp","remote_port":2222,"local_addr":"127.0.0.1:22","enabled":true}`
	resp := doClientRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/mappings", body, cookie, map[string]string{
		"X-Requested-With": "hy2tun",
		"Origin":           ts.URL,
	})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	readClientBody(t, resp)

	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/mappings/m1", body, cookie, map[string]string{
		"X-Requested-With": "hy2tun",
		"Origin":           ts.URL,
	})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	readClientBody(t, resp)
}

func TestClientWebRejectsBadMapping(t *testing.T) {
	api := &fakeClientAPI{}
	_, ts := newTestClientWeb(t, api)

	cookie := loginClientWeb(t, ts)
	body := `{"type":"tcp","remote_port":2222,"local_addr":"","enabled":true}`
	resp := doClientRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/mappings", body, cookie, map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Empty(t, api.added)
	readClientBody(t, resp)
}

func TestClientWebStatusAndLogs(t *testing.T) {
	api := &fakeClientAPI{
		status: view.ClientStatus{
			Name:      "home",
			Server:    "example.com:443",
			Version:   "v1.2.3",
			Connected: true,
			Uptime:    90 * time.Second,
			TxBytes:   1024,
			RxBytes:   2048,
		},
		mappings: []view.MappingView{
			{Mapping: config.Mapping{ID: "m1", Type: "tcp", Enabled: true}, Status: "running"},
			{Mapping: config.Mapping{ID: "m2", Type: "udp", Enabled: true}, Status: "failed"},
			{Mapping: config.Mapping{ID: "m3", Type: "tcp", Enabled: false}, Status: "disabled"},
		},
		logs: []view.LogEntry{
			{Time: time.Now(), Level: "info", Message: "started"},
			{Time: time.Now(), Level: "warn", Message: "careful"},
			{Time: time.Now(), Level: "error", Message: "boom"},
		},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/status", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	status := readClientJSON[statusResponse](t, resp)
	require.Equal(t, "home", status.Name)
	require.True(t, status.Connected)
	require.Equal(t, int64(90), status.UptimeSeconds)
	require.Equal(t, uint64(1024), status.TxBytes)
	require.Equal(t, 1, status.ActiveMappings)
	require.Equal(t, 3, status.Mappings)

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/logs?level=warn&limit=1", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	entries := readClientJSON[[]view.LogEntry](t, resp)
	require.Len(t, entries, 1)
	require.Equal(t, "boom", entries[0].Message)
	require.Equal(t, "warn", api.lastLevel)
	require.Equal(t, 1, api.lastLimit)

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/mappings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	mappings := readClientJSON[[]mappingResponse](t, resp)
	require.Len(t, mappings, 3)
	require.Equal(t, "m1", mappings[0].ID)
}

func TestClientServersPageAndSelection(t *testing.T) {
	us := &fakeClientAPI{
		status:   view.ClientStatus{Name: "us", Server: "us.example.com:443", Connected: true},
		mappings: []view.MappingView{{Mapping: config.Mapping{ID: "us-m", Type: "tcp", RemotePort: 25022, LocalAddr: "127.0.0.1:22", Enabled: true}}},
		settings: SettingsView{Name: "us"},
	}
	sg := &fakeClientAPI{
		status:   view.ClientStatus{Name: "sg", Server: "sg.example.com:443", Connected: false},
		settings: SettingsView{Name: "sg"},
	}
	multi := &fakeMultiAPI{
		servers: map[string]*fakeClientAPI{"us": us, "sg": sg},
		order:   []string{"us", "sg"},
	}
	_, ts := newTestClientWeb(t, multi)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/servers", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readClientBody(t, resp)
	require.Contains(t, body, "us")
	require.Contains(t, body, "sg")

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/?server=us", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body = readClientBody(t, resp)
	require.Contains(t, body, "us.example.com:443")
	require.Contains(t, body, "us-m")

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/?server=missing", "", cookie, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/status?server=us", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, readClientBody(t, resp), "us.example.com:443")

	// No default server means the old unscoped API must not guess.
	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/status", "", cookie, nil)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()

	body = `{"id":"new","type":"tcp","remote_port":25023,"local_addr":"127.0.0.1:23","enabled":true}`
	resp = doClientRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/mappings?server=sg", body, cookie, map[string]string{
		"X-Requested-With": "hy2tun",
		"Origin":           ts.URL,
	})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()
	require.Len(t, sg.added, 1)
	require.Empty(t, us.added)
}

func TestClientProxySettingsAPIAndCSRF(t *testing.T) {
	api := &fakeClientAPI{
		settings: SettingsView{Proxy: ProxyView{Enabled: false, SOCKS5: "127.0.0.1:1080"}},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/settings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, readClientBody(t, resp), `"socks5":"127.0.0.1:1080"`)

	body := `{"enabled":true,"socks5":"127.0.0.1:1081","http":"127.0.0.1:8081"}`
	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/proxy", body, cookie, map[string]string{
		"X-Requested-With": "hy2tun",
		"Origin":           ts.URL,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	require.True(t, api.settings.Proxy.Enabled)
	require.Equal(t, "127.0.0.1:1081", api.settings.Proxy.SOCKS5)

	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/proxy", body, cookie, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
}

func TestClientProxySettingsPageHasControls(t *testing.T) {
	api := &fakeClientAPI{settings: SettingsView{Proxy: ProxyView{SOCKS5: "127.0.0.1:1080"}}}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)
	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readClientBody(t, resp)
	require.Contains(t, body, `id="settings-proxy-form"`)
	require.Contains(t, body, `id="proxy-socks5"`)
	require.Contains(t, body, `id="proxy-http"`)
}

func TestClientProxySettingsMapsConflict(t *testing.T) {
	api := &fakeClientAPI{proxyErr: errors.New("proxy address conflict: 127.0.0.1:1080")}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)
	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/proxy",
		`{"enabled":true,"socks5":"127.0.0.1:1080"}`, cookie, map[string]string{
			"X-Requested-With": "hy2tun",
			"Origin":           ts.URL,
		})
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	require.Contains(t, readClientBody(t, resp), "proxy_conflict")
}

func TestClientWebPageRendersStatusTabsAndSettings(t *testing.T) {
	api := &fakeClientAPI{
		status: view.ClientStatus{
			Name:      "home",
			Server:    "example.com:443",
			Version:   "v9.9.9",
			Connected: true,
			Uptime:    2 * time.Hour,
			TxBytes:   2048,
			RxBytes:   4096,
		},
		mappings: []view.MappingView{{
			Mapping:      config.Mapping{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "192.168.1.10:22", Note: "NAS SSH", Enabled: true},
			Status:       "running",
			AssignedPort: 23001,
			TxBytes:      100,
			RxBytes:      200,
		}},
		settings: SettingsView{
			WebUsername:           "admin",
			WebPasswordConfigured: true,
			Name:                  "home",
			Credentials: CredentialSettingsView{
				Generation:                2,
				ServerGeneration:          3,
				CurrentAuthMasked:         MaskedSecret,
				CurrentControlTokenMasked: MaskedSecret,
				Synced:                    false,
			},
			ReadOnly: []ReadOnlySetting{
				{Label: "服务器地址", Value: "example.com:443"},
				{Label: "TLS", Value: "已启用 · SNI example.com"},
				{Label: "Obfs", Value: "salamander"},
				{Label: "配置文件", Value: "/etc/hy2tun/client.yaml"},
			},
		},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readClientBody(t, resp)

	require.Contains(t, body, "home")
	require.Contains(t, body, "example.com:443")
	require.Contains(t, body, "v9.9.9")
	require.Contains(t, body, "运行中")
	require.Contains(t, body, "端口映射")
	require.Contains(t, body, "日志")
	require.Contains(t, body, "设置")
	require.Contains(t, body, "类型")
	require.Contains(t, body, "服务器:端口")
	require.Contains(t, body, "内网目标")
	require.Contains(t, body, "备注")
	require.Contains(t, body, "流量")
	require.Contains(t, body, "NAS SSH")
	require.Contains(t, body, "23001")
	require.Contains(t, body, "••••••••")
	require.Contains(t, body, "/etc/hy2tun/client.yaml")
	require.Contains(t, body, "data-tab=\"mappings\"")
	require.Contains(t, body, "data-tab=\"logs\"")
	require.Contains(t, body, "data-tab=\"settings\"")
}

func TestClientWebRequiresAuthAndCSRF(t *testing.T) {
	api := &fakeClientAPI{}
	_, ts := newTestClientWeb(t, api)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/status", "", nil, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	cookie := loginClientWeb(t, ts)
	body := `{"type":"tcp","local_addr":"127.0.0.1:22","enabled":true}`
	resp = doClientRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/mappings", body, cookie, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// A custom header alone is not enough: a same-origin Origin is required.
	resp = doClientRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/mappings", body, cookie, map[string]string{"X-Requested-With": "hy2tun"})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// A cross-origin Origin is rejected even with the custom header.
	resp = doClientRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/mappings", body, cookie, map[string]string{
		"X-Requested-With": "hy2tun",
		"Origin":           "http://evil.example",
	})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	require.Empty(t, api.added)
}

func TestClientWebStaticAndLoginPage(t *testing.T) {
	_, ts := newTestClientWeb(t, &fakeClientAPI{})

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/static/app.css", "", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/css")
	require.Contains(t, readClientBody(t, resp), "--bg")

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/static/app.js", "", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, readClientBody(t, resp), "EventSource")

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/login", "", nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	require.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	require.Contains(t, resp.Header.Get("Content-Security-Policy"), "default-src 'self'")
	body := readClientBody(t, resp)
	require.Contains(t, body, `action="/api/login"`)
	require.Contains(t, body, "登录客户端控制台")
}

func TestClientWebHSTSBehindTLSProxy(t *testing.T) {
	web, _ := newTestClientWeb(t, &fakeClientAPI{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/login", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	recorder := httptest.NewRecorder()
	web.Handler().ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "max-age=31536000", recorder.Header().Get("Strict-Transport-Security"))

	req = httptest.NewRequest(http.MethodGet, "http://example.com/login", nil)
	recorder = httptest.NewRecorder()
	web.Handler().ServeHTTP(recorder, req)
	require.Empty(t, recorder.Header().Get("Strict-Transport-Security"))
}

func TestClientWebSSE(t *testing.T) {
	api := &fakeClientAPI{
		status: view.ClientStatus{Name: "home", Server: "example.com:443", Connected: true},
		mappings: []view.MappingView{{
			Mapping: config.Mapping{ID: "m1", Type: "tcp", LocalAddr: "127.0.0.1:22", Enabled: true},
			Status:  "running",
		}},
	}
	web, ts := newTestClientWeb(t, api)
	web.eventInterval = 20 * time.Millisecond
	cookie := loginClientWeb(t, ts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")

	reader := bufio.NewReader(resp.Body)
	event, data := readClientSSEEvent(t, reader)
	require.Equal(t, "snapshot", event)
	var snapshot sseSnapshot
	require.NoError(t, json.Unmarshal([]byte(data), &snapshot))
	require.Equal(t, "home", snapshot.Status.Name)
	require.Len(t, snapshot.Mappings, 1)
	require.Equal(t, "m1", snapshot.Mappings[0].ID)

	cancel()
}

func readClientSSEEvent(t *testing.T, reader *bufio.Reader) (string, string) {
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
		if event != "" || data != "" {
			return event, data
		}
	}
}

func TestClientWebStartAndClose(t *testing.T) {
	web, err := NewClient(&fakeClientAPI{}, config.WebConfig{
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

func TestClientWebStartTLS(t *testing.T) {
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

	web, err := NewClient(&fakeClientAPI{}, config.WebConfig{
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

func TestClientWebNewValidation(t *testing.T) {
	cfg := config.WebConfig{Listen: "127.0.0.1:0", Username: "admin", PasswordHash: mustHash("pw")}

	_, err := NewClient(nil, cfg, testSessionKey, slog.Default())
	require.Error(t, err)

	_, err = NewClient(&fakeClientAPI{}, cfg, []byte("short"), slog.Default())
	require.Error(t, err)

	emptyHash := cfg
	emptyHash.PasswordHash = ""
	_, err = NewClient(&fakeClientAPI{}, emptyHash, testSessionKey, slog.Default())
	require.Error(t, err)

	badHash := cfg
	badHash.PasswordHash = "not-a-bcrypt-hash"
	_, err = NewClient(&fakeClientAPI{}, badHash, testSessionKey, slog.Default())
	require.Error(t, err)

	badTLS := cfg
	badTLS.TLS = config.WebTLSConfig{Cert: "cert.pem"}
	_, err = NewClient(&fakeClientAPI{}, badTLS, testSessionKey, slog.Default())
	require.Error(t, err)
}

func TestClientWebRotateCredentials(t *testing.T) {
	api := &fakeClientAPI{}
	web, err := NewClient(api, config.WebConfig{
		Listen: "127.0.0.1:0", Username: "admin", PasswordHash: mustHash("pw"),
	}, testSessionKey, slog.Default())
	require.NoError(t, err)
	ts := httptest.NewServer(web.Handler())
	defer ts.Close()

	client := ts.Client()
	resp, err := client.PostForm(ts.URL+"/api/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NotEmpty(t, resp.Cookies())
	cookie := resp.Cookies()[0]
	resp.Body.Close()

	body := `{"current_password":"pw","generation":2,"auth":"new-auth","control_token":"new-token-0123456789abcdef"}`
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/settings/credentials", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "hy2tun")
	req.Header.Set("Origin", ts.URL)
	req.AddCookie(cookie)
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var accepted struct {
		Generation int64  `json:"generation"`
		Status     string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&accepted))
	resp.Body.Close()
	require.Equal(t, int64(2), accepted.Generation)
	require.Equal(t, "pending", accepted.Status)
	require.Equal(t, int64(2), api.rotation.Generation)

	// The settings API reflects the locally applied generation and remains
	// unsynced until the next hello_ack.
	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/settings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	settings := readClientJSON[SettingsView](t, resp)
	require.Equal(t, int64(2), settings.Credentials.Generation)
	require.False(t, settings.Credentials.Synced)
	require.Equal(t, MaskedSecret, settings.Credentials.CurrentAuthMasked)
	require.Equal(t, MaskedSecret, settings.Credentials.CurrentControlTokenMasked)
}

func TestClientWebRotateCredentialsRejectsShortAuth(t *testing.T) {
	api := &fakeClientAPI{}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	body := `{"current_password":"pw","generation":2,"auth":"short","control_token":"new-token-0123456789abcdef"}`
	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/credentials", body, cookie, map[string]string{
		"Content-Type":     "application/json",
		"X-Requested-With": "hy2tun",
		"Origin":           ts.URL,
	})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, readClientBody(t, resp), "weak_auth")
	require.Zero(t, api.rotation.Generation, "the handler must reject before writing config")
}

func TestHealthAndMetrics(t *testing.T) {
	api := &fakeClientAPI{
		status: view.ClientStatus{
			Connected:                  true,
			TxBytes:                    100,
			RxBytes:                    200,
			Uptime:                     90 * time.Second,
			CredentialGeneration:       2,
			ServerCredentialGeneration: 2,
			CredentialSynced:           true,
		},
		mappings: []view.MappingView{
			{Mapping: config.Mapping{ID: "m1"}},
			{Mapping: config.Mapping{ID: "m2"}},
		},
	}
	_, ts := newTestClientWeb(t, api)

	resp, err := ts.Client().Get(ts.URL + "/healthz")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "ok\n", string(body))

	resp, err = ts.Client().Get(ts.URL + "/metrics")
	require.NoError(t, err)
	body, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), "hy2tun_client_connected 1")
	require.Contains(t, string(body), "hy2tun_client_mappings 2")
	require.Contains(t, string(body), "hy2tun_client_tx_bytes_total 100")
	require.Contains(t, string(body), "hy2tun_client_rx_bytes_total 200")
	require.Contains(t, string(body), "hy2tun_client_uptime_seconds 90")
	require.Contains(t, string(body), "hy2tun_client_credential_synced 1")
}

func TestClientWebSettingsJSONAndPage(t *testing.T) {
	api := &fakeClientAPI{
		status: view.ClientStatus{Name: "home", Server: "example.com:443", Connected: true},
		settings: SettingsView{
			WebUsername:           "admin",
			WebPasswordConfigured: true,
			Name:                  "home",
			Credentials: CredentialSettingsView{
				Generation:                4,
				ServerGeneration:          5,
				CurrentAuthMasked:         MaskedSecret,
				CurrentControlTokenMasked: MaskedSecret,
			},
			ReadOnly: []ReadOnlySetting{
				{Label: "服务器地址", Value: "example.com:443"},
				{Label: "配置文件", Value: "/etc/hy2tun/client.yaml"},
			},
		},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/settings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	settings := readClientJSON[SettingsView](t, resp)
	require.Equal(t, "admin", settings.WebUsername)
	require.True(t, settings.WebPasswordConfigured)
	require.Equal(t, "home", settings.Name)
	require.Equal(t, int64(4), settings.Credentials.Generation)
	require.Equal(t, int64(5), settings.Credentials.ServerGeneration)
	require.Equal(t, MaskedSecret, settings.Credentials.CurrentAuthMasked)
	require.Equal(t, MaskedSecret, settings.Credentials.CurrentControlTokenMasked)
	require.Len(t, settings.ReadOnly, 2)

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readClientBody(t, resp)
	require.Contains(t, body, "Web 管理员")
	require.Contains(t, body, "已设置（只能重置，不会回显）")
	require.Contains(t, body, "HY2 凭据")
	require.Contains(t, body, "客户端名称")
	require.Contains(t, body, "保存并重连")
	require.Contains(t, body, "data-settings-generation")
	require.Contains(t, body, "data-server-generation")
	require.Contains(t, body, "data-reveal-credentials")
	require.Contains(t, body, "/etc/hy2tun/client.yaml")
	require.NotContains(t, body, "plain-auth")
	require.NotContains(t, body, "plain-token")
}

func TestClientWebRevealRequiresPasswordAndNoStore(t *testing.T) {
	api := &fakeClientAPI{
		settings: SettingsView{
			Credentials: CredentialSettingsView{
				Generation:                3,
				CurrentAuthMasked:         MaskedSecret,
				CurrentControlTokenMasked: MaskedSecret,
			},
		},
		rotation: credentials.Set{
			Generation:   3,
			Auth:         "plain-auth",
			ControlToken: "plain-token",
		},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/settings/credentials/reveal",
		`{"password":"wrong"}`, cookie, map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	body := readClientBody(t, resp)
	require.NotContains(t, body, "plain-auth")
	require.NotContains(t, body, "plain-token")

	resp = doClientRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/settings/credentials/reveal",
		`{"password":"pw"}`, cookie, map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	revealed := readClientJSON[revealCredentialsResponse](t, resp)
	require.NotNil(t, revealed.Current)
	require.Equal(t, int64(3), revealed.Current.Generation)
	require.Equal(t, "plain-auth", revealed.Current.Auth)
	require.Equal(t, "plain-token", revealed.Current.ControlToken)
}

func TestClientWebUpdateName(t *testing.T) {
	api := &fakeClientAPI{
		settings: SettingsView{Name: "old-name", Credentials: CredentialSettingsView{Generation: 1}},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/name",
		`{"current_password":"pw","name":"new-name"}`, cookie,
		map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "new-name", api.name)
	settings := readClientJSON[SettingsView](t, resp)
	require.Equal(t, "new-name", settings.Name)

	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/name",
		`{"current_password":"wrong","name":"ignored"}`, cookie,
		map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	readClientBody(t, resp)
	require.Equal(t, "new-name", api.name)
}

func TestClientWebUpdateWebConfigRevokesSessions(t *testing.T) {
	api := &fakeClientAPI{}
	web, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/web",
		`{"current_password":"pw","username":"ops","new_password":"newpassword","confirm_password":"newpassword"}`,
		cookie, map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()
	require.Equal(t, "ops", api.web.Username)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(api.web.PasswordHash), []byte("newpassword")))
	require.Equal(t, "ops", web.cfg.Username)
	require.Equal(t, api.web.PasswordHash, web.cfg.PasswordHash)

	// The old session cookie is revoked immediately.
	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/status", "", cookie, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// The old password no longer logs in; the new pair does.
	resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{"username": {"ops"}, "password": {"pw"}})
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	resp, err = ts.Client().PostForm(ts.URL+"/api/login", url.Values{"username": {"ops"}, "password": {"newpassword"}})
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NotEmpty(t, resp.Cookies())
	resp.Body.Close()

	// The page and settings API use the new administrator name.
	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/", "", resp.Cookies()[0], map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, readClientBody(t, resp), "ops")
	require.Equal(t, "ops", web.sessions.AdminUsername())
}

func TestClientWebSettingsRequireCSRFAndCurrentPassword(t *testing.T) {
	api := &fakeClientAPI{settings: SettingsView{Credentials: CredentialSettingsView{Generation: 2}}}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	// Missing CSRF header and Origin.
	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/name",
		`{"current_password":"pw","name":"new"}`, cookie, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()

	// Wrong current password.
	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/credentials",
		`{"current_password":"wrong","generation":3,"auth":"a","control_token":"b"}`, cookie,
		map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
	require.Zero(t, api.rotation.Generation)

	// A stale generation is rejected without touching the client.
	resp = doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/credentials",
		`{"current_password":"pw","generation":2,"auth":"a","control_token":"b"}`, cookie,
		map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	readClientBody(t, resp)
	require.Zero(t, api.rotation.Generation)

	// Reveal also requires the current password.
	resp = doClientRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/settings/credentials/reveal",
		`{"password":"wrong"}`, cookie, map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	resp.Body.Close()
}

func TestClientWebSettingsPasswordFailuresAreRateLimited(t *testing.T) {
	api := &fakeClientAPI{settings: SettingsView{Credentials: CredentialSettingsView{Generation: 1}}}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	headers := map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL}
	for i := 0; i < 5; i++ {
		resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/name",
			`{"current_password":"wrong","name":"new"}`, cookie, headers)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		resp.Body.Close()
	}
	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/name",
		`{"current_password":"pw","name":"new"}`, cookie, headers)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "60", resp.Header.Get("Retry-After"))
	resp.Body.Close()
}

func TestClientWebStatusIncludesCredentialState(t *testing.T) {
	api := &fakeClientAPI{
		status: view.ClientStatus{
			Name:                       "home",
			CredentialGeneration:       4,
			ServerCredentialGeneration: 4,
			CredentialSynced:           true,
		},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/status", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	status := readClientJSON[statusResponse](t, resp)
	require.Equal(t, int64(4), status.CredentialGeneration)
	require.Equal(t, int64(4), status.ServerCredentialGeneration)
	require.True(t, status.CredentialSynced)
}

func TestClientWebUpdateWebConfigFailureKeepsRuntimeAdmin(t *testing.T) {
	api := &fakeClientAPI{webErr: errors.New("save client config: disk full")}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/settings/web",
		`{"current_password":"pw","username":"ops","new_password":"newpassword","confirm_password":"newpassword"}`,
		cookie, map[string]string{"X-Requested-With": "hy2tun", "Origin": ts.URL})
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	readClientBody(t, resp)
	require.Empty(t, api.web.Username)

	// The failed write must leave the existing session and password intact.
	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/status", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()
}

func TestClientWebSettingsMasksPlaintextFromAPI(t *testing.T) {
	api := &fakeClientAPI{
		settings: SettingsView{
			Credentials: CredentialSettingsView{
				Generation:                2,
				CurrentAuthMasked:         "plain-auth",
				CurrentControlTokenMasked: "plain-token",
			},
		},
	}
	_, ts := newTestClientWeb(t, api)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/api/settings", "", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	settings := readClientJSON[SettingsView](t, resp)
	require.Equal(t, MaskedSecret, settings.Credentials.CurrentAuthMasked)
	require.Equal(t, MaskedSecret, settings.Credentials.CurrentControlTokenMasked)

	resp = doClientRequest(t, ts.Client(), http.MethodGet, ts.URL+"/", "", cookie, map[string]string{"Accept": "text/html"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readClientBody(t, resp)
	require.NotContains(t, body, "plain-auth")
	require.NotContains(t, body, "plain-token")
	require.Contains(t, body, MaskedSecret)
}

func TestClientProxyServerScopedRoute(t *testing.T) {
	us := &fakeClientAPI{settings: SettingsView{Name: "us"}}
	sg := &fakeClientAPI{settings: SettingsView{Name: "sg"}}
	multi := &fakeMultiAPI{
		servers: map[string]*fakeClientAPI{"us": us, "sg": sg},
		order:   []string{"us", "sg"},
	}
	_, ts := newTestClientWeb(t, multi)
	cookie := loginClientWeb(t, ts)

	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/servers/sg/proxy",
		`{"enabled":true,"socks5":"127.0.0.1:1082"}`, cookie, map[string]string{
			"X-Requested-With": "hy2tun",
			"Origin":           ts.URL,
		})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
	require.True(t, sg.settings.Proxy.Enabled)
	require.False(t, us.settings.Proxy.Enabled)
}
