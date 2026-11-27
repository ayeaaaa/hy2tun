package manager

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoginAndCrossOriginMutationRejected(t *testing.T) {
	store := testStore(t) // creates admin/admin hash with test password
	web, err := NewWebServer(store, testAudit(t), testConfig(), slog.Default())
	require.NoError(t, err)
	ts := httptest.NewServer(web.Handler())
	defer ts.Close()

	resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"}, "password": {"pw"},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	cookie := resp.Cookies()[0]

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/servers", strings.NewReader(`{}`))
	req.AddCookie(cookie)
	req.Header.Set("X-Requested-With", "hy2tun")
	req.Header.Set("Origin", "http://evil.example")
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestLoginJSONRendersOverviewAndLogoutRevokesSession(t *testing.T) {
	store := testStore(t)
	web, err := NewWebServer(store, testAudit(t), testConfig(), slog.Default())
	require.NoError(t, err)
	ts := httptest.NewServer(web.Handler())
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/api/login", "application/json",
		strings.NewReader(`{"username":"admin","password":"pw"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	cookie := resp.Cookies()[0]
	require.Equal(t, "hy2tun_manager_session", cookie.Name)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "hy2tun 管理器")
	require.Contains(t, body, "服务器清单")
	require.Contains(t, body, "退出登录")
	require.NotContains(t, strings.ToLower(body), "<iframe")

	// Logout without the CSRF header must not end the session.
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/api/logout", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	req, err = http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// A same-origin mutation with the static header succeeds and clears it.
	req, err = http.NewRequest(http.MethodPost, ts.URL+"/api/logout", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	req.Header.Set("X-Requested-With", "hy2tun")
	req.Header.Set("Origin", ts.URL)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	req, err = http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	noRedirect := ts.Client()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err = noRedirect.Do(req)
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/login", resp.Header.Get("Location"))
}

func TestLoginRejectsBadCredentialsAndRateLimitsWithoutLoggingSecrets(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store := testStore(t)
	web, err := NewWebServer(store, testAudit(t), testConfig(), logger)
	require.NoError(t, err)
	ts := httptest.NewServer(web.Handler())
	defer ts.Close()

	for attempt := 1; attempt <= 5; attempt++ {
		resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
			"username": {"admin"}, "password": {"not-the-password"},
		})
		require.NoError(t, err)
		body := readBody(t, resp)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		require.Contains(t, body, "invalid credentials")
		require.NotContains(t, body, "not-the-password")
		require.NotContains(t, body, "admin")
	}

	resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"}, "password": {"pw"},
	})
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "60", resp.Header.Get("Retry-After"))
	require.NotContains(t, logs.String(), "not-the-password")
	require.NotContains(t, logs.String(), "password")
}

func TestLoginPageStaticAssetsAndSecurityHeaders(t *testing.T) {
	store := testStore(t)
	web, err := NewWebServer(store, testAudit(t), testConfig(), slog.Default())
	require.NoError(t, err)
	ts := httptest.NewServer(web.Handler())
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/login")
	require.NoError(t, err)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, "hy2tun 管理器")
	require.Contains(t, body, `action="/api/login"`)
	require.Contains(t, body, `name="csrf-header"`)
	require.Contains(t, body, `href="/static/app.css"`)
	require.NotContains(t, strings.ToLower(body), "<iframe")
	require.Equal(t, "DENY", resp.Header.Get("X-Frame-Options"))
	require.Contains(t, resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'")
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))

	resp, err = ts.Client().Get(ts.URL + "/static/app.css")
	require.NoError(t, err)
	cssBody := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "text/css")
	require.NotEmpty(t, cssBody)

	resp, err = ts.Client().Get(ts.URL + "/static/app.js")
	require.NoError(t, err)
	jsBody := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "javascript")
	require.Contains(t, jsBody, "csrf-header")
}

func TestWebServerHonorsBasePath(t *testing.T) {
	store := testStore(t)
	cfg := testConfig()
	cfg.BasePath = "/manager/"
	web, err := NewWebServer(store, testAudit(t), cfg, slog.Default())
	require.NoError(t, err)
	ts := httptest.NewServer(web.Handler())
	defer ts.Close()

	noRedirect := ts.Client()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Get(ts.URL + "/manager")
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	require.Equal(t, "/manager/", resp.Header.Get("Location"))

	resp, err = ts.Client().Get(ts.URL + "/manager/login")
	require.NoError(t, err)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, body, `href="/manager/static/app.css"`)
	require.Contains(t, body, `action="/manager/api/login"`)

	resp, err = ts.Client().PostForm(ts.URL+"/manager/api/login", url.Values{
		"username": {"admin"}, "password": {"pw"},
	})
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	cookie := resp.Cookies()[0]

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/manager/", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(data)
}
