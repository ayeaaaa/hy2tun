package clientui

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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
	settings Settings

	addErr    error
	updateErr error
	deleteErr error

	added   []config.Mapping
	updated []config.Mapping
	deleted []string

	lastLevel string
	lastLimit int
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

func (f *fakeClientAPI) Settings() Settings {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings
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
	resp := doClientRequest(t, ts.Client(), http.MethodPut, ts.URL+"/api/mappings/m1", body, cookie, map[string]string{"X-Requested-With": "hy2tun"})
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
	resp := doClientRequest(t, ts.Client(), http.MethodDelete, ts.URL+"/api/mappings/m1", "", cookie, map[string]string{"X-Requested-With": "hy2tun"})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Equal(t, []string{"m1"}, api.deleted)
	resp.Body.Close()
}

func TestClientWebRejectsBadMapping(t *testing.T) {
	api := &fakeClientAPI{}
	_, ts := newTestClientWeb(t, api)

	cookie := loginClientWeb(t, ts)
	body := `{"type":"tcp","remote_port":2222,"local_addr":"","enabled":true}`
	resp := doClientRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/mappings", body, cookie, map[string]string{"X-Requested-With": "hy2tun"})
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
		settings: Settings{
			Server:       "example.com:443",
			TLS:          true,
			TLSInsecure:  false,
			TLSSNI:       "example.com",
			Obfs:         "salamander",
			ControlToken: "••••••••",
			ConfigPath:   "/etc/hy2tun/client.yaml",
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
	resp = doClientRequest(t, ts.Client(), http.MethodPost, ts.URL+"/api/mappings", `{"type":"tcp","local_addr":"127.0.0.1:22","enabled":true}`, cookie, nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Empty(t, api.added)
	resp.Body.Close()
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
