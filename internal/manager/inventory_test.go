package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeUpstream is a minimal hy2tund API used by the inventory and handler
// tests. It enforces a real session cookie so the adapter's login path is
// exercised, and it records request/concurrency counters for local-removal and
// single-flight assertions.
type fakeUpstream struct {
	server    *httptest.Server
	closeOnce sync.Once

	mu                  sync.Mutex
	requestCount        int
	loginCount          int
	overviewInFlight    int
	overviewMaxInFlight int
	overviewDelay       time.Duration
	rejectAll           bool
	acceptedPasswords   map[string]bool
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	fake := &fakeUpstream{
		acceptedPasswords: map[string]bool{"upstream-pw": true, "new-pw": true},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.close)
	return fake
}

// BaseURL includes a path prefix so the inventory must rely on the upstream
// adapter's existing URL joining instead of assembling paths itself.
func (f *fakeUpstream) BaseURL() string { return f.server.URL + "/hy2tund" }

func (f *fakeUpstream) close() { f.closeOnce.Do(f.server.Close) }

func (f *fakeUpstream) setRejectAll(reject bool) {
	f.mu.Lock()
	f.rejectAll = reject
	f.mu.Unlock()
}

func (f *fakeUpstream) setOverviewDelay(delay time.Duration) {
	f.mu.Lock()
	f.overviewDelay = delay
	f.mu.Unlock()
}

func (f *fakeUpstream) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requestCount
}

func (f *fakeUpstream) logins() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginCount
}

func (f *fakeUpstream) maxOverviewInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.overviewMaxInFlight
}

func (f *fakeUpstream) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requestCount++
	rejectAll := f.rejectAll
	delay := f.overviewDelay
	f.mu.Unlock()

	path := r.URL.Path
	if strings.HasPrefix(path, "/hy2tund") {
		path = strings.TrimPrefix(path, "/hy2tund")
	}
	if !strings.HasPrefix(path, "/api/") {
		http.NotFound(w, r)
		return
	}

	if rejectAll {
		writeTestJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized", "code": "unauthorized"})
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
		accepted := f.acceptedPasswords[r.PostForm.Get("password")] && r.PostForm.Get("username") == "admin"
		f.mu.Unlock()
		if !accepted {
			writeTestJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid credentials", "code": "unauthorized"})
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
		f.mu.Lock()
		f.overviewInFlight++
		if f.overviewInFlight > f.overviewMaxInFlight {
			f.overviewMaxInFlight = f.overviewInFlight
		}
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		defer func() {
			f.mu.Lock()
			f.overviewInFlight--
			f.mu.Unlock()
		}()
		writeTestJSON(w, http.StatusOK, map[string]any{
			"online_clients":  2,
			"active_mappings": 5,
			"listen":          ":443",
			"uptime_seconds":  3600,
			"tx_bytes":        1024,
			"rx_bytes":        2048,
		})
	case "/api/clients":
		writeTestJSON(w, http.StatusOK, []map[string]any{
			{
				"ID":       "c1",
				"Name":     "laptop",
				"Online":   true,
				"Revision": 1,
				"Mappings": []map[string]any{
					{"id": "m1", "type": "tcp", "remote_port": 8080, "enabled": true},
				},
			},
			{
				"ID":       "c2",
				"Name":     "phone",
				"Online":   false,
				"Revision": 1,
				"Mappings": []map[string]any{},
			},
		})
	case "/api/config/summary":
		writeTestJSON(w, http.StatusOK, map[string]any{
			"version": "v1.7.4",
			"mode":    "hy2+tcp",
			"listen":  ":443",
		})
	default:
		writeTestJSON(w, http.StatusNotFound, map[string]any{"error": "not found", "code": "not_found"})
	}
}

func writeTestJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func testInventory(t *testing.T) (*Inventory, *Store, *AuditLog) {
	t.Helper()
	store := testStore(t)
	audit := testAudit(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewInventory(store, audit, NewUpstreamClient, log), store, audit
}

func addServerRequest(name, baseURL string) AddServerRequest {
	return AddServerRequest{
		Name:     name,
		BaseURL:  baseURL,
		Username: "admin",
		Password: "upstream-pw",
	}
}

func storeRaw(t *testing.T, inv *Inventory) string {
	t.Helper()
	data, err := os.ReadFile(inv.store.statePath)
	require.NoError(t, err)
	return string(data)
}

func auditEntries(t *testing.T, audit *AuditLog) []AuditEntry {
	t.Helper()
	data, err := os.ReadFile(audit.path)
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	entries := make([]AuditEntry, 0)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry AuditEntry
		require.NoError(t, json.Unmarshal([]byte(line), &entry))
		entries = append(entries, entry)
	}
	return entries
}

func findServerRecord(t *testing.T, store *Store, name string) ServerRecord {
	t.Helper()
	for _, record := range store.State().Servers {
		if record.Name == name {
			return record
		}
	}
	t.Fatalf("server %q not found in state", name)
	return ServerRecord{}
}

func TestInventoryAddTestsLoginEncryptsPasswordAndRefreshes(t *testing.T) {
	upstream := newFakeUpstream(t)
	inv, store, audit := testInventory(t)

	view, err := inv.Add(context.Background(), addServerRequest("us", upstream.BaseURL()))
	require.NoError(t, err)
	require.Equal(t, "online", view.State)
	require.Equal(t, "v1.7.4", view.Version)
	require.Equal(t, "hy2+tcp", view.Mode)
	require.Equal(t, 2, view.ClientCount)
	require.Equal(t, 5, view.MappingCount)
	require.False(t, view.LastRefresh.IsZero())
	require.Empty(t, view.LastError)
	require.GreaterOrEqual(t, upstream.logins(), 1)

	raw := storeRaw(t, inv)
	require.NotContains(t, raw, "upstream-pw")
	record := findServerRecord(t, store, "us")
	require.NotEmpty(t, record.UpstreamPasswordEnc)
	password, err := store.ServerPassword(record.ID)
	require.NoError(t, err)
	require.Equal(t, "upstream-pw", string(password))

	entries := auditEntries(t, audit)
	require.NotEmpty(t, entries)
	require.Equal(t, "server.add", entries[len(entries)-1].Action)
	require.NotContains(t, readAuditRaw(t, audit), "upstream-pw")
}

func TestInventoryAuditFailureBlocksAddAndRemove(t *testing.T) {
	upstream := newFakeUpstream(t)
	store := testStore(t)
	brokenAudit := NewAuditLog(filepath.Join(t.TempDir(), "missing", "audit.jsonl"))
	inv := NewInventory(store, brokenAudit, NewUpstreamClient, slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := inv.Add(context.Background(), addServerRequest("us", upstream.BaseURL()))
	require.Error(t, err)
	require.Empty(t, store.State().Servers, "audit failure must leave inventory untouched")

	workingAudit := testAudit(t)
	workingInventory := NewInventory(store, workingAudit, NewUpstreamClient, slog.New(slog.NewTextHandler(io.Discard, nil)))
	view, err := workingInventory.Add(context.Background(), addServerRequest("us", upstream.BaseURL()))
	require.NoError(t, err)
	workingInventory.audit = brokenAudit
	require.Error(t, workingInventory.Remove(view.ID))
	require.Len(t, store.State().Servers, 1, "audit failure must block removal")
}

func TestInventoryAddValidatesInputsAndDuplicateNames(t *testing.T) {
	upstream := newFakeUpstream(t)
	inv, store, _ := testInventory(t)

	invalid := []AddServerRequest{
		{BaseURL: upstream.BaseURL(), Username: "admin", Password: "upstream-pw"},
		{Name: "x", BaseURL: "", Username: "admin", Password: "upstream-pw"},
		{Name: "x", BaseURL: "ftp://example.test", Username: "admin", Password: "upstream-pw"},
		{Name: "x", BaseURL: upstream.BaseURL(), Password: "upstream-pw"},
		{Name: "x", BaseURL: upstream.BaseURL(), Username: "admin"},
	}
	for _, request := range invalid {
		_, err := inv.Add(context.Background(), request)
		require.Error(t, err, "request %+v should be rejected", request)
		var validation *ValidationError
		require.ErrorAs(t, err, &validation)
	}
	require.Empty(t, store.State().Servers)

	first, err := inv.Add(context.Background(), addServerRequest("us", upstream.BaseURL()))
	require.NoError(t, err)
	_, err = inv.Add(context.Background(), addServerRequest("us", upstream.BaseURL()))
	require.Error(t, err)
	var validation *ValidationError
	require.ErrorAs(t, err, &validation)
	require.Len(t, store.State().Servers, 1)

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	view, err := inv.Add(context.Background(), addServerRequest("dead", deadURL))
	require.Error(t, err)
	require.Equal(t, "unreachable", view.State)
	require.NotEmpty(t, view.LastError)
	require.Len(t, store.State().Servers, 1)
	require.Equal(t, "us", inv.List()[0].Name)
	require.NotEqual(t, first.ID, "")
}

func TestInventoryUpdateRejectsDuplicateNameAndFailedCredentialTest(t *testing.T) {
	upstream := newFakeUpstream(t)
	inv, store, _ := testInventory(t)
	_, err := inv.Add(context.Background(), addServerRequest("a", upstream.BaseURL()))
	require.NoError(t, err)
	second, err := inv.Add(context.Background(), addServerRequest("b", upstream.BaseURL()))
	require.NoError(t, err)
	original := findServerRecord(t, store, "b")

	err = inv.Update(context.Background(), second.ID, UpdateServerRequest{
		Name: "a", BaseURL: upstream.BaseURL(), Username: "admin",
	})
	require.Error(t, err)
	var validation *ValidationError
	require.ErrorAs(t, err, &validation)
	require.Equal(t, "b", findServerRecord(t, store, "b").Name)

	err = inv.Update(context.Background(), second.ID, UpdateServerRequest{
		Name: "b", BaseURL: upstream.BaseURL(), Username: "admin", Password: "wrong-pw",
	})
	require.Error(t, err)
	var connection *ServerConnectionError
	require.ErrorAs(t, err, &connection)
	require.Equal(t, "auth_error", connection.State)
	afterFailedTest := findServerRecord(t, store, "b")
	require.Equal(t, original.UpstreamPasswordEnc, afterFailedTest.UpstreamPasswordEnc)
	require.Equal(t, "b", afterFailedTest.Name)
	password, err := store.ServerPassword(second.ID)
	require.NoError(t, err)
	require.Equal(t, "upstream-pw", string(password))

	err = inv.Update(context.Background(), second.ID, UpdateServerRequest{
		Name: "b2", BaseURL: upstream.BaseURL(), Username: "admin",
	})
	require.NoError(t, err)
	renamed := findServerRecord(t, store, "b2")
	require.Equal(t, original.UpstreamPasswordEnc, renamed.UpstreamPasswordEnc)
	view, ok := inv.Get(second.ID)
	require.True(t, ok)
	require.Equal(t, "b2", view.Name)
	require.Equal(t, "v1.7.4", view.Version)

	err = inv.Update(context.Background(), second.ID, UpdateServerRequest{
		Name: "b2", BaseURL: upstream.BaseURL(), Username: "admin", Password: "new-pw",
	})
	require.NoError(t, err)
	password, err = store.ServerPassword(second.ID)
	require.NoError(t, err)
	require.Equal(t, "new-pw", string(password))
	require.Equal(t, "b2", findServerRecord(t, store, "b2").Name)
}

func TestInventoryRemoveIsLocalAndAudited(t *testing.T) {
	upstream := newFakeUpstream(t)
	inv, store, audit := testInventory(t)
	view, err := inv.Add(context.Background(), addServerRequest("us", upstream.BaseURL()))
	require.NoError(t, err)
	before := upstream.requests()

	require.NoError(t, inv.Remove(view.ID))
	require.Empty(t, store.State().Servers)
	_, ok := inv.Get(view.ID)
	require.False(t, ok)
	require.Equal(t, before, upstream.requests(), "removing inventory must not call the upstream server")

	entries := auditEntries(t, audit)
	require.NotEmpty(t, entries)
	last := entries[len(entries)-1]
	require.Equal(t, "server.remove", last.Action)
	require.Equal(t, view.ID, last.ServerID)
	require.Equal(t, "us", last.ServerName)
	require.Equal(t, "ok", last.Result)

	require.Error(t, inv.Remove(view.ID))
}

func TestInventoryRefreshClassifiesErrorsAndKeepsInventory(t *testing.T) {
	upstream := newFakeUpstream(t)
	inv, store, _ := testInventory(t)
	view, err := inv.Add(context.Background(), addServerRequest("us", upstream.BaseURL()))
	require.NoError(t, err)

	upstream.setRejectAll(true)
	refreshed, err := inv.Refresh(context.Background(), view.ID)
	require.Error(t, err)
	require.Equal(t, "auth_error", refreshed.State)
	require.NotEmpty(t, refreshed.LastError)
	require.Len(t, store.State().Servers, 1)
	require.Equal(t, "v1.7.4", refreshed.Version, "last known data must remain visible")

	upstream.setRejectAll(false)
	refreshed, err = inv.Refresh(context.Background(), view.ID)
	require.NoError(t, err)
	require.Equal(t, "online", refreshed.State)
	require.Empty(t, refreshed.LastError)

	upstream.setOverviewDelay(80 * time.Millisecond)
	inv.refreshTimeout = 20 * time.Millisecond
	refreshed, err = inv.Refresh(context.Background(), view.ID)
	require.Error(t, err)
	require.Equal(t, "unreachable", refreshed.State)
	require.Len(t, store.State().Servers, 1)

	upstream.setOverviewDelay(0)
	inv.refreshTimeout = 15 * time.Second
	upstream.close()
	refreshed, err = inv.Refresh(context.Background(), view.ID)
	require.Error(t, err)
	require.Equal(t, "unreachable", refreshed.State)
	require.Len(t, store.State().Servers, 1)
	require.Equal(t, view.ID, store.State().Servers[0].ID)
}

func TestInventoryRefreshSerializesPerServer(t *testing.T) {
	upstream := newFakeUpstream(t)
	inv, _, _ := testInventory(t)
	view, err := inv.Add(context.Background(), addServerRequest("us", upstream.BaseURL()))
	require.NoError(t, err)
	upstream.setOverviewDelay(15 * time.Millisecond)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = inv.Refresh(context.Background(), view.ID)
		}()
	}
	wg.Wait()
	require.Equal(t, 1, upstream.maxOverviewInFlight())
}

func TestInventoryRefreshAllIsBounded(t *testing.T) {
	upstream := newFakeUpstream(t)
	inv, _, _ := testInventory(t)
	upstream.setOverviewDelay(10 * time.Millisecond)
	for index := range 6 {
		_, err := inv.Add(context.Background(), addServerRequest(fmt.Sprintf("srv-%d", index), upstream.BaseURL()))
		require.NoError(t, err)
	}
	upstream.mu.Lock()
	upstream.overviewMaxInFlight = 0
	upstream.mu.Unlock()

	inv.RefreshAll(context.Background())

	require.Greater(t, upstream.maxOverviewInFlight(), 0)
	require.LessOrEqual(t, upstream.maxOverviewInFlight(), inventoryRefreshConcurrency)
	for _, server := range inv.List() {
		require.Equal(t, "online", server.State)
	}
}

func readAuditRaw(t *testing.T, audit *AuditLog) string {
	t.Helper()
	data, err := os.ReadFile(audit.path)
	if os.IsNotExist(err) {
		return ""
	}
	require.NoError(t, err)
	return string(data)
}
