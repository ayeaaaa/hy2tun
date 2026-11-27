package manager

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestManagerSessionUsesDistinctCookieAndRejectsTamper(t *testing.T) {
	sessions, err := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	require.NoError(t, sessions.Issue(rr, req, "admin"))
	cookie := rr.Result().Cookies()[0]
	require.Equal(t, "hy2tun_manager_session", cookie.Name)
	require.True(t, cookie.HttpOnly)
	req.AddCookie(cookie)
	require.True(t, sessions.Valid(req))
	cookie.Value += "x"
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(cookie)
	require.False(t, sessions.Valid(req2))
}

func TestManagerSessionCookieAttributesAndSecureScheme(t *testing.T) {
	sessions, err := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	require.NoError(t, err)

	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	require.NoError(t, sessions.Issue(rr, plain, "admin"))
	cookie := rr.Result().Cookies()[0]
	require.Equal(t, "/", cookie.Path)
	require.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	require.False(t, cookie.Secure)
	require.Equal(t, int(time.Hour/time.Second), cookie.MaxAge)

	forwarded := httptest.NewRequest(http.MethodGet, "/", nil)
	forwarded.Header.Set("X-Forwarded-Proto", "https")
	rr = httptest.NewRecorder()
	require.NoError(t, sessions.Issue(rr, forwarded, "admin"))
	require.True(t, rr.Result().Cookies()[0].Secure)

	withTLS := httptest.NewRequest(http.MethodGet, "/", nil)
	withTLS.TLS = &tls.ConnectionState{}
	rr = httptest.NewRecorder()
	require.NoError(t, sessions.Issue(rr, withTLS, "admin"))
	require.True(t, rr.Result().Cookies()[0].Secure)
}

func TestManagerSessionClearRevokesCookieAndUsername(t *testing.T) {
	sessions, err := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	require.NoError(t, sessions.Issue(rr, req, "admin"))
	cookie := rr.Result().Cookies()[0]

	authed := httptest.NewRequest(http.MethodGet, "/", nil)
	authed.AddCookie(cookie)
	require.Equal(t, "admin", sessions.Username(authed))

	rr = httptest.NewRecorder()
	sessions.Clear(rr, authed)
	cleared := rr.Result().Cookies()[0]
	require.Equal(t, "hy2tun_manager_session", cleared.Name)
	require.Equal(t, "", cleared.Value)
	require.True(t, cleared.MaxAge < 0)

	replay := httptest.NewRequest(http.MethodGet, "/", nil)
	replay.AddCookie(cookie)
	require.False(t, sessions.Valid(replay))
	require.Equal(t, "", sessions.Username(replay))
}

func TestManagerSessionExpiresAndRejectsWeakKeys(t *testing.T) {
	_, err := NewSessionManager([]byte("short"), time.Hour)
	require.Error(t, err)

	sessions, err := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Minute)
	require.NoError(t, err)
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	sessions.now = func() time.Time { return now }

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	require.NoError(t, sessions.Issue(rr, req, "admin"))
	cookie := rr.Result().Cookies()[0]

	authed := httptest.NewRequest(http.MethodGet, "/", nil)
	authed.AddCookie(cookie)
	require.True(t, sessions.Valid(authed))

	now = now.Add(2 * time.Minute)
	require.False(t, sessions.Valid(authed))
}

func TestManagerSessionUsesIndependentRevocations(t *testing.T) {
	sessions, err := NewSessionManager([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	require.NoError(t, err)

	issue := func() *http.Cookie {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()
		require.NoError(t, sessions.Issue(rr, req, "admin"))
		return rr.Result().Cookies()[0]
	}
	first := issue()
	second := issue()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(first)
	rr := httptest.NewRecorder()
	sessions.Clear(rr, req)

	check := func(cookie *http.Cookie) bool {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(cookie)
		return sessions.Valid(r)
	}
	require.False(t, check(first))
	require.True(t, check(second))
}
