package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	managerupstream "hy2tun/internal/manager/upstream"
	"hy2tun/internal/view"
)

type fakeMappingPush struct {
	ClientID     string
	BaseRevision int64
	Mappings     []config.Mapping
	Exposures    map[string]view.Exposure
}

type fakeExposurePush struct {
	ClientID  string
	MappingID string
	Exposure  view.Exposure
}

// fakeClientUpstream is a stateful hy2tund client/mapping API used by the Task 5
// handler and serialization tests. It enforces the real login cookie and API
// path prefix, and records mapping pushes so tests can assert the exact
// replacement list and exposure map that the manager forwarded.
type fakeClientUpstream struct {
	server    *httptest.Server
	closeOnce sync.Once

	mu                 sync.Mutex
	loginCount         int
	clientGetCount     int
	clients            map[string]managerupstream.ClientView
	pushRequests       []fakeMappingPush
	exposureRequests   []fakeExposurePush
	pushStatus         int
	pushCode           string
	pushMessage        string
	exposureStatus     int
	exposureCode       string
	exposureMessage    string
	overviewGate       <-chan struct{}
	overviewStarted    chan struct{}
	overviewStartTaken bool
}

func newFakeClientUpstream(t *testing.T) *fakeClientUpstream {
	t.Helper()
	fake := &fakeClientUpstream{
		clients: map[string]managerupstream.ClientView{
			"c1": {
				ID:       "c1",
				Name:     "laptop",
				Version:  "v1.7.4",
				Revision: 7,
				Online:   true,
				LastSeen: time.Now().UTC(),
				Mappings: []managerupstream.MappingView{
					{
						Mapping: config.Mapping{
							ID:         "m1",
							Type:       "tcp",
							RemotePort: 8080,
							LocalAddr:  "127.0.0.1:22",
							Note:       "ssh",
							Enabled:    true,
						},
						Status:       "running",
						AssignedPort: 8080,
						Exposure:     view.ExposurePublic,
					},
				},
			},
			"c2": {
				ID:       "c2",
				Name:     "phone",
				Revision: 2,
				Online:   false,
				LastSeen: time.Now().UTC().Add(-time.Hour),
				Mappings: []managerupstream.MappingView{},
			},
		},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.close)
	return fake
}

func (f *fakeClientUpstream) BaseURL() string { return f.server.URL + fakeUpstreamBasePath }

func (f *fakeClientUpstream) close() { f.closeOnce.Do(f.server.Close) }

func (f *fakeClientUpstream) failMappings(status int, code, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushStatus = status
	f.pushCode = code
	f.pushMessage = message
}

func (f *fakeClientUpstream) failExposure(status int, code, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exposureStatus = status
	f.exposureCode = code
	f.exposureMessage = message
}

func (f *fakeClientUpstream) setOnline(clientID string, online bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	client := f.clients[clientID]
	client.Online = online
	f.clients[clientID] = client
}

func (f *fakeClientUpstream) logins() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginCount
}

func (f *fakeClientUpstream) clientGets() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clientGetCount
}

func (f *fakeClientUpstream) pushes() []fakeMappingPush {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeMappingPush, len(f.pushRequests))
	copy(out, f.pushRequests)
	return out
}

func (f *fakeClientUpstream) exposures() []fakeExposurePush {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeExposurePush, len(f.exposureRequests))
	copy(out, f.exposureRequests)
	return out
}

// blockOverview installs a one-shot gate on GET /api/overview. The caller waits
// for started, then releases the handler with the returned function.
func (f *fakeClientUpstream) blockOverview() (<-chan struct{}, func()) {
	gate := make(chan struct{})
	started := make(chan struct{})
	f.mu.Lock()
	f.overviewGate = gate
	f.overviewStarted = started
	f.overviewStartTaken = false
	f.mu.Unlock()
	return started, func() { close(gate) }
}

func (f *fakeClientUpstream) handle(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, fakeUpstreamBasePath+"/") {
		http.NotFound(w, r)
		return
	}
	path = strings.TrimPrefix(path, fakeUpstreamBasePath)
	if !strings.HasPrefix(path, "/api/") {
		http.NotFound(w, r)
		return
	}

	if path == "/api/login" {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": "bad form"})
			return
		}
		f.mu.Lock()
		f.loginCount++
		accepted := r.PostForm.Get("password") == "upstream-pw" &&
			r.PostForm.Get("username") == "admin"
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
		f.waitOverviewGate()
		f.mu.Lock()
		online := 0
		mappings := 0
		for _, client := range f.clients {
			if client.Online {
				online++
			}
			mappings += len(client.Mappings)
		}
		f.mu.Unlock()
		writeTestJSON(w, http.StatusOK, map[string]any{
			"online_clients":  online,
			"active_mappings": mappings,
			"listen":          ":443",
			"uptime_seconds":  3600,
		})
		return
	case "/api/config/summary":
		writeTestJSON(w, http.StatusOK, map[string]any{
			"version": "v1.7.4",
			"mode":    "hy2+tcp",
			"listen":  ":443",
		})
		return
	case "/api/clients":
		f.mu.Lock()
		clients := make([]managerupstream.ClientView, 0, len(f.clients))
		for _, client := range f.clients {
			clients = append(clients, cloneClientView(client))
		}
		f.mu.Unlock()
		writeTestJSON(w, http.StatusOK, clients)
		return
	}

	if !strings.HasPrefix(path, "/api/clients/") {
		writeTestJSON(w, http.StatusNotFound, map[string]any{"error": "not found", "code": "not_found"})
		return
	}
	rest := strings.TrimPrefix(path, "/api/clients/")
	parts := strings.Split(rest, "/")
	clientID := parts[0]

	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		f.mu.Lock()
		f.clientGetCount++
		client, ok := f.clients[clientID]
		f.mu.Unlock()
		if !ok {
			writeTestJSON(w, http.StatusNotFound, map[string]any{"error": "client not found", "code": "client_not_found"})
			return
		}
		writeTestJSON(w, http.StatusOK, cloneClientView(client))
	case len(parts) == 2 && parts[1] == "mappings" && r.Method == http.MethodPut:
		f.handlePushMappings(w, r, clientID)
	case len(parts) == 4 && parts[1] == "mappings" && parts[3] == "exposure" && r.Method == http.MethodPut:
		f.handleExposure(w, r, clientID, parts[2])
	default:
		writeTestJSON(w, http.StatusNotFound, map[string]any{"error": "not found", "code": "not_found"})
	}
}

func (f *fakeClientUpstream) waitOverviewGate() {
	f.mu.Lock()
	gate := f.overviewGate
	started := f.overviewStarted
	taken := f.overviewStartTaken
	if started != nil && !taken {
		f.overviewStartTaken = true
	}
	f.mu.Unlock()
	if started != nil && !taken {
		close(started)
	}
	if gate != nil {
		<-gate
	}
}

func (f *fakeClientUpstream) handlePushMappings(w http.ResponseWriter, r *http.Request, clientID string) {
	var payload struct {
		BaseRevision int64                    `json:"base_revision"`
		Mappings     []config.Mapping         `json:"mappings"`
		Exposures    map[string]view.Exposure `json:"exposures"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body", "code": "invalid_body"})
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pushStatus != 0 {
		status := f.pushStatus
		code := f.pushCode
		message := f.pushMessage
		writeTestJSON(w, status, map[string]any{"error": message, "code": code})
		return
	}
	client, ok := f.clients[clientID]
	if !ok {
		writeTestJSON(w, http.StatusNotFound, map[string]any{"error": "client not found", "code": "client_not_found"})
		return
	}
	if payload.BaseRevision != client.Revision {
		writeTestJSON(w, http.StatusConflict, map[string]any{
			"error": "客户端配置已更新", "code": managerupstream.CodeConflict,
		})
		return
	}
	f.pushRequests = append(f.pushRequests, fakeMappingPush{
		ClientID:     clientID,
		BaseRevision: payload.BaseRevision,
		Mappings:     append([]config.Mapping(nil), payload.Mappings...),
		Exposures:    cloneExposures(payload.Exposures),
	})
	client.Mappings = applyMappingPush(client.Mappings, payload.Mappings, payload.Exposures)
	client.Revision++
	f.clients[clientID] = client
	writeTestJSON(w, http.StatusOK, map[string]any{"revision": client.Revision})
}

func (f *fakeClientUpstream) handleExposure(w http.ResponseWriter, r *http.Request, clientID, mappingID string) {
	var payload struct {
		Exposure view.Exposure `json:"exposure"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeTestJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body", "code": "invalid_body"})
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exposureStatus != 0 {
		status := f.exposureStatus
		code := f.exposureCode
		message := f.exposureMessage
		writeTestJSON(w, status, map[string]any{"error": message, "code": code})
		return
	}
	client, ok := f.clients[clientID]
	if !ok {
		writeTestJSON(w, http.StatusNotFound, map[string]any{"error": "client not found", "code": "client_not_found"})
		return
	}
	for index := range client.Mappings {
		if client.Mappings[index].ID != mappingID {
			continue
		}
		f.exposureRequests = append(f.exposureRequests, fakeExposurePush{
			ClientID: clientID, MappingID: mappingID, Exposure: payload.Exposure,
		})
		client.Mappings[index].Exposure = payload.Exposure
		f.clients[clientID] = client
		writeTestJSON(w, http.StatusOK, client.Mappings[index])
		return
	}
	writeTestJSON(w, http.StatusNotFound, map[string]any{"error": "mapping not found", "code": "mapping_not_found"})
}

func cloneClientView(client managerupstream.ClientView) managerupstream.ClientView {
	client.Mappings = append([]managerupstream.MappingView(nil), client.Mappings...)
	for index := range client.Mappings {
		client.Mappings[index].UnavailableFamilies = append([]string(nil), client.Mappings[index].UnavailableFamilies...)
	}
	return client
}

func cloneExposures(exposures map[string]view.Exposure) map[string]view.Exposure {
	if len(exposures) == 0 {
		return nil
	}
	out := make(map[string]view.Exposure, len(exposures))
	for key, value := range exposures {
		out[key] = value
	}
	return out
}

func applyMappingPush(existing []managerupstream.MappingView, mappings []config.Mapping, exposures map[string]view.Exposure) []managerupstream.MappingView {
	byID := make(map[string]managerupstream.MappingView, len(existing))
	for _, mapping := range existing {
		byID[mapping.ID] = mapping
	}
	out := make([]managerupstream.MappingView, 0, len(mappings))
	for _, mapping := range mappings {
		mappingView := byID[mapping.ID]
		mappingView.Mapping = mapping
		if exposure, ok := exposures[mapping.ID]; ok {
			mappingView.Exposure = exposure
		} else if mappingView.Exposure == "" {
			mappingView.Exposure = view.ExposurePublic
		}
		if mappingView.Status == "" {
			mappingView.Status = "pending"
		}
		if mappingView.AssignedPort == 0 {
			mappingView.AssignedPort = mapping.RemotePort
		}
		out = append(out, mappingView)
	}
	return out
}

func TestClientMappingPageAndSuccessfulPush(t *testing.T) {
	upstream := newFakeClientUpstream(t)
	env := newManagerTestEnv(t)

	resp, body := env.postJSON(t, "/api/servers", addServerPayload("us", upstream.BaseURL(), "upstream-pw"))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var server ServerView
	require.NoError(t, json.Unmarshal([]byte(body), &server))

	status, page := env.getPage(t, "/servers/"+server.ID+"/clients")
	require.Equal(t, http.StatusOK, status, page)
	require.Contains(t, page, "laptop")
	require.Contains(t, page, "c1")
	require.Contains(t, page, "在线")
	require.Contains(t, page, "离线")
	require.Contains(t, page, "/servers/"+server.ID+"/clients/c1")

	status, page = env.getPage(t, "/servers/"+server.ID+"/clients/c1")
	require.Equal(t, http.StatusOK, status, page)
	require.Contains(t, page, "laptop")
	require.Contains(t, page, "m1")
	require.Contains(t, page, "8080")
	require.Contains(t, page, "127.0.0.1:22")
	require.Contains(t, page, "公网")
	require.Contains(t, page, "运行中")
	require.Contains(t, page, `data-base-revision="7"`)
	require.NotContains(t, page, "upstream-pw")

	status, page = env.getPage(t, "/servers/"+server.ID+"/clients/c2")
	require.Equal(t, http.StatusOK, status, page)
	require.Contains(t, page, "data-mappings-table", "the add form must render with an empty mapping table")
	require.Contains(t, page, "该客户端还没有映射")

	payload := map[string]any{
		"base_revision": 7,
		"mappings": []map[string]any{
			{
				"id": "m1", "type": "tcp", "remote_port": 8080,
				"local_addr": "127.0.0.1:22", "note": "ssh edited", "enabled": true,
			},
			{
				"id": "m2", "type": "udp", "remote_port": 0,
				"local_addr": "127.0.0.1:53", "note": "dns", "enabled": true,
			},
		},
		"exposures": map[string]any{"m2": "local"},
	}
	resp, body = env.putJSON(t, "/api/servers/"+server.ID+"/clients/c1/mappings", payload)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var refreshed managerupstream.ClientView
	require.NoError(t, json.Unmarshal([]byte(body), &refreshed))
	require.Equal(t, int64(8), refreshed.Revision)
	require.Len(t, refreshed.Mappings, 2)

	pushes := upstream.pushes()
	require.Len(t, pushes, 1)
	require.Equal(t, "c1", pushes[0].ClientID)
	require.Equal(t, int64(7), pushes[0].BaseRevision)
	require.Len(t, pushes[0].Mappings, 2)
	require.Equal(t, "ssh edited", pushes[0].Mappings[0].Note)
	require.Equal(t, view.ExposureLocal, pushes[0].Exposures["m2"])

	status, page = env.getPage(t, "/servers/"+server.ID+"/clients/c1")
	require.Equal(t, http.StatusOK, status, page)
	require.Contains(t, page, "dns")
	require.Contains(t, page, "仅本机")
	require.NotContains(t, page, "upstream-pw")

	entries := auditEntries(t, env.audit)
	require.GreaterOrEqual(t, len(entries), 2)
	last := entries[len(entries)-1]
	require.Equal(t, "mapping.update", last.Action)
	require.Equal(t, "ok", last.Result)
	require.Equal(t, server.ID, last.ServerID)
	require.Contains(t, last.Target, "c1")
	require.Contains(t, last.Target, "m2")
	require.NotContains(t, readAuditRaw(t, env.audit), "upstream-pw")

	// Deleting the whole list must still name the removed mappings in the
	// terminal audit entry.
	resp, body = env.putJSON(t, "/api/servers/"+server.ID+"/clients/c1/mappings", map[string]any{
		"base_revision": 8,
		"mappings":      []map[string]any{},
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	entries = auditEntries(t, env.audit)
	last = entries[len(entries)-1]
	require.Equal(t, "mapping.update", last.Action)
	require.Equal(t, "ok", last.Result)
	require.Contains(t, last.Target, "m1")
	require.Contains(t, last.Target, "m2")
}

func TestClientMappingConflictAndOfflineRules(t *testing.T) {
	upstream := newFakeClientUpstream(t)
	env := newManagerTestEnv(t)

	resp, body := env.postJSON(t, "/api/servers", addServerPayload("us", upstream.BaseURL(), "upstream-pw"))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var server ServerView
	require.NoError(t, json.Unmarshal([]byte(body), &server))
	endpoint := "/api/servers/" + server.ID + "/clients/c1/mappings"

	// Missing CSRF must be rejected before any upstream mapping write.
	noCSRF, err := http.NewRequest(http.MethodPut, env.server.URL+endpoint, strings.NewReader(`{"base_revision":7,"mappings":[]}`))
	require.NoError(t, err)
	noCSRF.AddCookie(env.cookie)
	noCSRF.Header.Set("Content-Type", "application/json")
	resp, err = env.server.Client().Do(noCSRF)
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Empty(t, upstream.pushes())

	upstream.failMappings(http.StatusConflict, managerupstream.CodeConflict, "客户端配置已更新，请刷新后重试")
	resp, body = env.putJSON(t, endpoint, map[string]any{
		"base_revision": 7,
		"mappings": []map[string]any{{
			"id": "m1", "type": "tcp", "remote_port": 8080,
			"local_addr": "127.0.0.1:22", "note": "stale", "enabled": true,
		}},
	})
	require.Equal(t, http.StatusConflict, resp.StatusCode, body)
	var conflict struct {
		Error  string `json:"error"`
		Code   string `json:"code"`
		Reload bool   `json:"reload"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &conflict))
	require.Equal(t, managerupstream.CodeConflict, conflict.Code)
	require.Contains(t, conflict.Error, "刷新")
	require.True(t, conflict.Reload)
	require.Empty(t, upstream.pushes(), "a conflict must not be retried or forwarded twice")

	upstream.failExposure(http.StatusConflict, managerupstream.CodeExposureConflict, "映射暴露范围切换失败，已保留当前状态")
	resp, body = env.putJSON(t, endpoint+"/m1/exposure", map[string]any{"exposure": "local"})
	require.Equal(t, http.StatusConflict, resp.StatusCode, body)
	require.Contains(t, body, managerupstream.CodeExposureConflict)
	require.Empty(t, upstream.exposures())

	upstream.failMappings(http.StatusConflict, managerupstream.CodeClientOffline, "客户端离线，无法修改配置")
	resp, body = env.putJSON(t, endpoint, map[string]any{
		"base_revision": 7,
		"mappings": []map[string]any{{
			"id": "m1", "type": "tcp", "remote_port": 8080,
			"local_addr": "127.0.0.1:22", "note": "offline", "enabled": true,
		}},
	})
	require.Equal(t, http.StatusConflict, resp.StatusCode, body)
	require.Contains(t, body, managerupstream.CodeClientOffline)
	require.Empty(t, upstream.pushes())

	// A client that the fresh detail reports as offline disables mapping
	// controls without blocking the exposure-only server-side operation.
	upstream.setOnline("c1", false)
	status, page := env.getPage(t, "/servers/"+server.ID+"/clients/c1")
	require.Equal(t, http.StatusOK, status, page)
	require.Contains(t, page, "离线客户端不可编辑")
	require.Contains(t, page, `data-mapping-save-list`)
	require.Contains(t, page, "disabled")
	require.Contains(t, page, "暴露范围仍可调整")

	upstream.failExposure(0, "", "")
	resp, body = env.putJSON(t, endpoint+"/m1/exposure", map[string]any{"exposure": "local"})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	exposures := upstream.exposures()
	require.Len(t, exposures, 1)
	require.Equal(t, "m1", exposures[0].MappingID)
	require.Equal(t, view.ExposureLocal, exposures[0].Exposure)

	entries := auditEntries(t, env.audit)
	require.NotEmpty(t, entries)
	var sawConflict, sawOffline, sawExposure bool
	for _, entry := range entries {
		if entry.Action == "mapping.update" && entry.Result == "error" && entry.Code == managerupstream.CodeConflict {
			sawConflict = true
		}
		if entry.Action == "mapping.update" && entry.Result == "error" && entry.Code == managerupstream.CodeClientOffline {
			sawOffline = true
		}
		if entry.Action == "mapping.exposure" && entry.Result == "ok" {
			sawExposure = true
			require.Contains(t, entry.Target, "c1")
			require.Contains(t, entry.Target, "m1")
		}
	}
	require.True(t, sawConflict)
	require.True(t, sawOffline)
	require.True(t, sawExposure)
	require.NotContains(t, readAuditRaw(t, env.audit), "upstream-pw")
}

func TestClientMappingAuditFailureBlocksUpstreamWrite(t *testing.T) {
	upstream := newFakeClientUpstream(t)
	env := newManagerTestEnv(t)

	resp, body := env.postJSON(t, "/api/servers", addServerPayload("us", upstream.BaseURL(), "upstream-pw"))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var server ServerView
	require.NoError(t, json.Unmarshal([]byte(body), &server))

	env.inventory.audit = NewAuditLog(filepath.Join(t.TempDir(), "missing", "audit.jsonl"))
	resp, body = env.putJSON(t, "/api/servers/"+server.ID+"/clients/c1/mappings", map[string]any{
		"base_revision": 7,
		"mappings": []map[string]any{{
			"id": "m1", "type": "tcp", "remote_port": 8080,
			"local_addr": "127.0.0.1:22", "note": "blocked", "enabled": true,
		}},
	})
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode, body)
	require.Contains(t, body, "audit")
	require.Empty(t, upstream.pushes(), "an audit failure must block the upstream mutation")
}

func TestConcurrentRefreshAndMappingWriteSerializePerServer(t *testing.T) {
	upstream := newFakeClientUpstream(t)
	env := newManagerTestEnv(t)

	resp, body := env.postJSON(t, "/api/servers", addServerPayload("us", upstream.BaseURL(), "upstream-pw"))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var server ServerView
	require.NoError(t, json.Unmarshal([]byte(body), &server))

	started, release := upstream.blockOverview()
	refreshRequest, err := http.NewRequest(http.MethodPost,
		env.server.URL+"/api/servers/"+server.ID+"/refresh", nil)
	require.NoError(t, err)
	refreshRequest.AddCookie(env.cookie)
	refreshRequest.Header.Set("X-Requested-With", "hy2tun")
	refreshRequest.Header.Set("Origin", env.server.URL)

	refreshDone := make(chan error, 1)
	go func() {
		response, err := env.server.Client().Do(refreshRequest)
		if err == nil {
			discardResponseBody(response)
			if response.StatusCode != http.StatusOK {
				err = fmt.Errorf("refresh status %d", response.StatusCode)
			}
		}
		refreshDone <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not reach the upstream overview request")
	}

	payload, err := json.Marshal(map[string]any{
		"base_revision": 7,
		"mappings": []map[string]any{{
			"id": "m1", "type": "tcp", "remote_port": 8080,
			"local_addr": "127.0.0.1:22", "note": "concurrent", "enabled": true,
		}},
	})
	require.NoError(t, err)
	pushRequest, err := http.NewRequest(http.MethodPut,
		env.server.URL+"/api/servers/"+server.ID+"/clients/c1/mappings", strings.NewReader(string(payload)))
	require.NoError(t, err)
	pushRequest.AddCookie(env.cookie)
	pushRequest.Header.Set("X-Requested-With", "hy2tun")
	pushRequest.Header.Set("Origin", env.server.URL)
	pushRequest.Header.Set("Content-Type", "application/json")

	pushDone := make(chan error, 1)
	go func() {
		response, err := env.server.Client().Do(pushRequest)
		if err == nil {
			discardResponseBody(response)
			if response.StatusCode != http.StatusOK {
				err = fmt.Errorf("mapping push status %d", response.StatusCode)
			}
		}
		pushDone <- err
	}()

	time.Sleep(120 * time.Millisecond)
	require.Zero(t, len(upstream.pushes()), "mapping write must wait for the blocked refresh")
	before := upstream.clientGets()
	time.Sleep(40 * time.Millisecond)
	require.Equal(t, before, upstream.clientGets(), "no fresh client read may race the refresh")

	release()
	select {
	case err := <-refreshDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not complete after release")
	}
	select {
	case err := <-pushDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("mapping push did not complete after refresh")
	}
	pushes := upstream.pushes()
	require.Len(t, pushes, 1)
	require.Equal(t, int64(7), pushes[0].BaseRevision)
	require.Equal(t, "concurrent", pushes[0].Mappings[0].Note)
}

func TestInventoryWithClientReusesAndRevalidatesClient(t *testing.T) {
	upstream := newFakeUpstream(t)
	inv, store, _ := testInventory(t)
	view, err := inv.Add(context.Background(), addServerRequest("us", upstream.BaseURL()))
	require.NoError(t, err)

	loginsBefore := upstream.logins()
	var first *managerupstream.Client
	require.NoError(t, inv.WithClient(context.Background(), view.ID, func(ctx context.Context, client *managerupstream.Client) error {
		first = client
		_, err := client.Overview(ctx)
		return err
	}))
	require.NotNil(t, first)
	entry := inv.entries[view.ID]
	require.NotNil(t, entry)
	entry.mu.Lock()
	cached := entry.client
	entry.mu.Unlock()
	require.Same(t, cached, first, "WithClient must reuse the identity-checked cached client")
	require.Equal(t, loginsBefore, upstream.logins())

	var second *managerupstream.Client
	require.NoError(t, inv.WithClient(context.Background(), view.ID, func(ctx context.Context, client *managerupstream.Client) error {
		second = client
		_, err := client.Overview(ctx)
		return err
	}))
	require.Same(t, first, second)
	require.Equal(t, loginsBefore, upstream.logins(), "the cached client must keep its session")

	err = inv.WithClient(context.Background(), "srv_missing", func(context.Context, *managerupstream.Client) error {
		t.Fatal("callback must not run for an unknown server")
		return nil
	})
	require.ErrorIs(t, err, ErrServerNotFound)

	sentinel := errors.New("callback failed")
	err = inv.WithClient(context.Background(), view.ID, func(context.Context, *managerupstream.Client) error {
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	lock := inv.operationLock(view.ID)
	lock.Lock()
	callbackRan := make(chan struct{}, 1)
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- inv.WithClient(context.Background(), view.ID, func(context.Context, *managerupstream.Client) error {
			callbackRan <- struct{}{}
			return nil
		})
	}()
	select {
	case <-callbackRan:
		t.Fatal("WithClient must wait for the per-server operation lock")
	case <-time.After(80 * time.Millisecond):
	}
	lock.Unlock()
	select {
	case err := <-waitDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("WithClient did not resume after the lock was released")
	}

	secondUpstream := newFakeUpstream(t)
	secondUpstream.setConfigVersion("v1.8.0")
	record := findServerRecord(t, store, "us")
	record.BaseURL = secondUpstream.BaseURL()
	require.NoError(t, store.UpdateServer(record, nil))

	var third *managerupstream.Client
	require.NoError(t, inv.WithClient(context.Background(), view.ID, func(ctx context.Context, client *managerupstream.Client) error {
		third = client
		_, err := client.Overview(ctx)
		return err
	}))
	require.NotSame(t, first, third, "a direct store identity change must recreate the upstream client")
	require.Equal(t, 1, secondUpstream.logins())
}

func discardResponseBody(response *http.Response) {
	if response == nil {
		return
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
}

func TestClientPagesRequireAuthAndRenderWithNilInventory(t *testing.T) {
	store := testStore(t)
	audit := testAudit(t)
	web, err := NewWebServer(store, audit, testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	ts := httptest.NewServer(web.Handler())
	t.Cleanup(ts.Close)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(ts.URL + "/servers/srv_missing/clients")
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, "unauthenticated pages must redirect to login")
}
