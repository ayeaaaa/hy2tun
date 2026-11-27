package manager

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type managerTestEnv struct {
	inventory *Inventory
	store     *Store
	audit     *AuditLog
	server    *httptest.Server
	cookie    *http.Cookie
}

func newManagerTestEnv(t *testing.T) *managerTestEnv {
	t.Helper()
	store := testStore(t)
	audit := testAudit(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	inventory := NewInventory(store, audit, NewUpstreamClient, log)
	web, err := NewWebServer(store, audit, testConfig(), log)
	require.NoError(t, err)
	web.SetInventory(inventory)
	ts := httptest.NewServer(web.Handler())
	t.Cleanup(ts.Close)

	resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"},
		"password": {"pw"},
	})
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NotEmpty(t, resp.Cookies())

	return &managerTestEnv{
		inventory: inventory,
		store:     store,
		audit:     audit,
		server:    ts,
		cookie:    resp.Cookies()[0],
	}
}

func (e *managerTestEnv) newRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	require.NoError(t, err)
	req.AddCookie(e.cookie)
	if method != http.MethodGet {
		req.Header.Set("X-Requested-With", "hy2tun")
		req.Header.Set("Origin", e.server.URL)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func (e *managerTestEnv) request(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	resp, err := e.server.Client().Do(e.newRequest(t, method, path, body))
	require.NoError(t, err)
	return resp
}

func (e *managerTestEnv) getPage(t *testing.T, path string) (int, string) {
	t.Helper()
	resp := e.request(t, http.MethodGet, path, "")
	return resp.StatusCode, readBody(t, resp)
}

func (e *managerTestEnv) postJSON(t *testing.T, path string, value any) (*http.Response, string) {
	t.Helper()
	payload, err := json.Marshal(value)
	require.NoError(t, err)
	resp := e.request(t, http.MethodPost, path, string(payload))
	return resp, readBody(t, resp)
}

func (e *managerTestEnv) putJSON(t *testing.T, path string, value any) (*http.Response, string) {
	t.Helper()
	payload, err := json.Marshal(value)
	require.NoError(t, err)
	resp := e.request(t, http.MethodPut, path, string(payload))
	return resp, readBody(t, resp)
}

func addServerPayload(name, baseURL, password string) map[string]any {
	return map[string]any{
		"name":            name,
		"base_url":        baseURL,
		"username":        "admin",
		"password":        password,
		"tls_skip_verify": false,
	}
}

func TestServerHandlersAddListAndDetail(t *testing.T) {
	upstream := newFakeUpstream(t)
	env := newManagerTestEnv(t)

	resp, body := env.postJSON(t, "/api/servers", addServerPayload("us", upstream.BaseURL(), "upstream-pw"))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var view ServerView
	require.NoError(t, json.Unmarshal([]byte(body), &view))
	require.NotEmpty(t, view.ID)
	require.Equal(t, "us", view.Name)
	require.Equal(t, "online", view.State)
	require.Equal(t, "v1.7.4", view.Version)
	require.Equal(t, 2, view.ClientCount)
	require.Equal(t, 5, view.MappingCount)
	require.NotContains(t, body, "upstream-pw")

	status, page := env.getPage(t, "/")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, page, "服务器清单")
	require.Contains(t, page, "us")
	require.Contains(t, page, upstream.BaseURL())
	require.Contains(t, page, "在线")
	require.Contains(t, page, "v1.7.4")
	require.Contains(t, page, "HY2")
	require.Contains(t, page, "刷新全部")
	require.NotContains(t, page, "upstream-pw")

	status, page = env.getPage(t, "/servers")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, page, `name="password"`)
	require.Contains(t, page, `type="password"`)
	require.Contains(t, page, `autocomplete="new-password"`)
	require.Contains(t, page, upstream.BaseURL())
	require.NotContains(t, page, "upstream-pw")

	status, page = env.getPage(t, "/servers/"+view.ID)
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, page, "us")
	require.Contains(t, page, upstream.BaseURL())
	require.Contains(t, page, "v1.7.4")
	require.NotContains(t, page, "upstream-pw")

	status, body = env.getPage(t, "/api/servers")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, view.ID)
	require.NotContains(t, body, "upstream-pw")

	status, _ = env.getPage(t, "/servers/srv_missing")
	require.Equal(t, http.StatusNotFound, status)

	resp, body = env.postJSON(t, "/api/servers", addServerPayload("us", upstream.BaseURL(), "upstream-pw"))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, body, "duplicate")
	resp, body = env.postJSON(t, "/api/servers", addServerPayload("US", upstream.BaseURL(), "upstream-pw"))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, body, "duplicate")
	resp, body = env.postJSON(t, "/api/servers", addServerPayload("", upstream.BaseURL(), "upstream-pw"))
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, body, "name")
}

func TestServerHandlerAcceptsFormEncodedAdd(t *testing.T) {
	upstream := newFakeUpstream(t)
	env := newManagerTestEnv(t)

	form := url.Values{
		"name":            {"eu"},
		"base_url":        {upstream.BaseURL()},
		"username":        {"admin"},
		"password":        {"upstream-pw"},
		"tls_skip_verify": {"false"},
	}
	req := env.newRequest(t, http.MethodPost, "/api/servers", form.Encode())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := env.server.Client().Do(req)
	require.NoError(t, err)
	body := readBody(t, resp)
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var view ServerView
	require.NoError(t, json.Unmarshal([]byte(body), &view))
	require.Equal(t, "eu", view.Name)
	require.Equal(t, "online", view.State)
	require.False(t, findServerRecord(t, env.store, "eu").TLSSkipVerify)
	require.NotContains(t, body, "upstream-pw")
}

func TestServerHandlerTestConnectionDoesNotSaveOrOverwriteCredentials(t *testing.T) {
	upstream := newFakeUpstream(t)
	env := newManagerTestEnv(t)

	resp, body := env.postJSON(t, "/api/servers/test", addServerPayload("probe", upstream.BaseURL(), "upstream-pw"))
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Empty(t, env.store.State().Servers)

	resp, body = env.postJSON(t, "/api/servers/test", addServerPayload("probe", upstream.BaseURL(), "wrong-pw"))
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, body)
	require.Contains(t, body, "auth_error")
	require.NotContains(t, body, "wrong-pw")
	require.Empty(t, env.store.State().Servers)

	resp, body = env.postJSON(t, "/api/servers", addServerPayload("us", upstream.BaseURL(), "upstream-pw"))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var added ServerView
	require.NoError(t, json.Unmarshal([]byte(body), &added))
	original := findServerRecord(t, env.store, "us")

	resp, body = env.postJSON(t, "/api/servers/"+added.ID+"/test", map[string]any{"password": "wrong-pw"})
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, body)
	require.Contains(t, body, "auth_error")
	require.NotContains(t, body, "wrong-pw")
	afterTest := findServerRecord(t, env.store, "us")
	require.Equal(t, original.UpstreamPasswordEnc, afterTest.UpstreamPasswordEnc)
	view, ok := env.inventory.Get(added.ID)
	require.True(t, ok)
	require.Equal(t, "online", view.State, "a failed test must not overwrite the live view")

	resp, body = env.putJSON(t, "/api/servers/"+added.ID, addServerPayload("changed", upstream.BaseURL(), "wrong-pw"))
	require.Equal(t, http.StatusBadGateway, resp.StatusCode, body)
	require.NotContains(t, body, "wrong-pw")
	afterUpdate := findServerRecord(t, env.store, "us")
	require.Equal(t, original.UpstreamPasswordEnc, afterUpdate.UpstreamPasswordEnc)
	require.Equal(t, "us", afterUpdate.Name)

	resp, body = env.putJSON(t, "/api/servers/"+added.ID, map[string]any{
		"name": "us2", "base_url": upstream.BaseURL(), "username": "admin", "password": "",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	renamed := findServerRecord(t, env.store, "us2")
	require.Equal(t, original.UpstreamPasswordEnc, renamed.UpstreamPasswordEnc)

	resp, body = env.postJSON(t, "/api/servers/"+added.ID+"/test", map[string]any{"password": ""})
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, "online")
	require.NotContains(t, body, "upstream-pw")
}

func TestServerHandlerRemoveRequiresConfirmationAndAudits(t *testing.T) {
	upstream := newFakeUpstream(t)
	env := newManagerTestEnv(t)

	resp, body := env.postJSON(t, "/api/servers", addServerPayload("us", upstream.BaseURL(), "upstream-pw"))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var view ServerView
	require.NoError(t, json.Unmarshal([]byte(body), &view))
	beforeRemoval := upstream.requests()

	resp = env.request(t, http.MethodDelete, "/api/servers/"+view.ID, "")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	readBody(t, resp)
	require.Len(t, env.store.State().Servers, 1)

	resp = env.request(t, http.MethodDelete, "/api/servers/"+view.ID+"?confirm=other", "")
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	readBody(t, resp)
	require.Len(t, env.store.State().Servers, 1)

	// A missing CSRF header must be rejected before removal.
	req := env.newRequest(t, http.MethodDelete, "/api/servers/"+view.ID+"?confirm=us", "")
	req.Header.Del("X-Requested-With")
	resp, err := env.server.Client().Do(req)
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Len(t, env.store.State().Servers, 1)

	resp = env.request(t, http.MethodDelete, "/api/servers/"+view.ID+"?confirm=us", "")
	readBody(t, resp)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Empty(t, env.store.State().Servers)
	require.Equal(t, beforeRemoval, upstream.requests(), "remove must be local-only")

	entries := auditEntries(t, env.audit)
	require.NotEmpty(t, entries)
	last := entries[len(entries)-1]
	require.Equal(t, "server.remove", last.Action)
	require.Equal(t, view.ID, last.ServerID)
	require.Equal(t, "us", last.ServerName)
	require.Equal(t, "admin", last.Actor)
	require.NotContains(t, readAuditRaw(t, env.audit), "upstream-pw")
}

func TestServerListShowsConfiguredServerWhenUpstreamUnreachable(t *testing.T) {
	upstream := newFakeUpstream(t)
	env := newManagerTestEnv(t)

	resp, body := env.postJSON(t, "/api/servers", addServerPayload("us", upstream.BaseURL(), "upstream-pw"))
	require.Equal(t, http.StatusCreated, resp.StatusCode, body)
	var view ServerView
	require.NoError(t, json.Unmarshal([]byte(body), &view))
	upstream.close()

	resp, body = env.postJSON(t, "/api/servers/"+view.ID+"/refresh", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	require.Contains(t, body, "unreachable")
	require.Equal(t, view.ID, env.store.State().Servers[0].ID)

	status, page := env.getPage(t, "/")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, page, "us")
	require.Contains(t, page, "无法连接")
	require.Contains(t, page, "v1.7.4")
	require.NotContains(t, page, "upstream-pw")

	status, page = env.getPage(t, "/servers")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, page, "us")
	require.Contains(t, page, "无法连接")
}

func TestServerPagesRenderWithNilInventory(t *testing.T) {
	store := testStore(t)
	audit := testAudit(t)
	web, err := NewWebServer(store, audit, testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	ts := httptest.NewServer(web.Handler())
	t.Cleanup(ts.Close)

	resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"},
		"password": {"pw"},
	})
	require.NoError(t, err)
	readBody(t, resp)
	cookie := resp.Cookies()[0]

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/servers", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "服务器清单未配置")

	req, err = http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	body = readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "服务器清单")
}
