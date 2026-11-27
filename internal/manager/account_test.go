package manager

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestAccountPasswordPageAndChangeInvalidatesSessions(t *testing.T) {
	store := testStore(t)
	audit := testAudit(t)
	web, err := NewWebServer(store, audit, testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	ts := httptest.NewServer(web.Handler())
	defer ts.Close()

	resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"}, "password": {"pw"},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	cookie := resp.Cookies()[0]

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/account", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	page := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, page, "修改管理员密码")
	require.Contains(t, page, `name="current_password"`)
	require.Contains(t, page, `name="new_password"`)
	require.Contains(t, page, `name="confirm_password"`)
	require.Contains(t, page, "修改后所有已登录的管理器会话都会失效")

	const newPassword = "new-password-123"
	body := `{"current_password":"pw","new_password":"` + newPassword + `","confirm_password":"` + newPassword + `"}`
	req, err = http.NewRequest(http.MethodPut, ts.URL+"/api/account/password", strings.NewReader(body))
	require.NoError(t, err)
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "hy2tun")
	req.Header.Set("Origin", ts.URL)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	state := store.State()
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(state.Admin.PasswordHash), []byte(newPassword)))
	require.Error(t, bcrypt.CompareHashAndPassword([]byte(state.Admin.PasswordHash), []byte("pw")))

	// The old session is invalid, and the old password cannot log in.
	noRedirect := ts.Client()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err = http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = noRedirect.Do(req)
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/login", resp.Header.Get("Location"))

	resp, err = ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"}, "password": {"pw"},
	})
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp, err = ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"}, "password": {newPassword},
	})
	require.NoError(t, err)
	readBody(t, resp)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	auditData, err := readAuditFile(audit)
	require.NoError(t, err)
	require.Contains(t, auditData, `"action":"manager.password"`)
	require.Contains(t, auditData, `"result":"ok"`)
	require.NotContains(t, auditData, "new-password-123")
	require.NotContains(t, auditData, `"pw"`)
}

func TestAccountPasswordRejectsWrongCurrentAndInvalidNew(t *testing.T) {
	store := testStore(t)
	web, err := NewWebServer(store, testAudit(t), testConfig(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	ts := httptest.NewServer(web.Handler())
	defer ts.Close()

	resp, err := ts.Client().PostForm(ts.URL+"/api/login", url.Values{
		"username": {"admin"}, "password": {"pw"},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	cookie := resp.Cookies()[0]
	before := store.State().Admin.PasswordHash

	putPassword := func(body string) int {
		req, err := http.NewRequest(http.MethodPut, ts.URL+"/api/account/password", strings.NewReader(body))
		require.NoError(t, err)
		req.AddCookie(cookie)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "hy2tun")
		req.Header.Set("Origin", ts.URL)
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		readBody(t, resp)
		return resp.StatusCode
	}

	require.Equal(t, http.StatusForbidden, putPassword(
		`{"current_password":"wrong","new_password":"new-password-123","confirm_password":"new-password-123"}`))
	require.Equal(t, before, store.State().Admin.PasswordHash)

	require.Equal(t, http.StatusBadRequest, putPassword(
		`{"current_password":"pw","new_password":"short","confirm_password":"short"}`))
	require.Equal(t, before, store.State().Admin.PasswordHash)

	require.Equal(t, http.StatusBadRequest, putPassword(
		`{"current_password":"pw","new_password":"new-password-123","confirm_password":"different-password"}`))
	require.Equal(t, before, store.State().Admin.PasswordHash)
}

func readAuditFile(audit *AuditLog) (string, error) {
	data, err := os.ReadFile(audit.path)
	return string(data), err
}
