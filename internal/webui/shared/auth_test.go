package shared

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func testSecret() []byte {
	return []byte("0123456789abcdef0123456789abcdef")
}

func TestNewSessionManagerRejectsWeakSecret(t *testing.T) {
	_, err := NewSessionManager(nil, time.Hour)
	require.Error(t, err)
	_, err = NewSessionManager([]byte(strings.Repeat("a", 31)), time.Hour)
	require.Error(t, err)
	_, err = NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)
}

func TestSessionManagerIssueAndValidate(t *testing.T) {
	manager, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)

	issueReq := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	recorder := httptest.NewRecorder()
	require.NoError(t, manager.Issue(recorder, issueReq, "admin"))

	cookies := recorder.Result().Cookies()
	require.Len(t, cookies, 1)
	cookie := cookies[0]
	require.Equal(t, CookieName, cookie.Name)
	require.Equal(t, "/", cookie.Path)
	require.True(t, cookie.HttpOnly)
	require.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
	require.False(t, cookie.Secure)
	require.InDelta(t, int(time.Hour/time.Second), cookie.MaxAge, 1)
	require.NotEmpty(t, cookie.Value)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.AddCookie(cookie)
	require.True(t, manager.Valid(req))
	require.Equal(t, "admin", manager.Username(req))

	// A tampered signature must be rejected.
	tampered := *cookie
	tampered.Value = cookie.Value + "x"
	req = httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.AddCookie(&tampered)
	require.False(t, manager.Valid(req))

	// A tampered payload must be rejected even if the signature is valid
	// base64.
	parts := strings.SplitN(cookie.Value, ".", 2)
	require.Len(t, parts, 2)
	payload := []byte(parts[0])
	if payload[0] == 'e' {
		payload[0] = 'f'
	} else {
		payload[0] = 'e'
	}
	tampered.Value = string(payload) + "." + parts[1]
	req = httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.AddCookie(&tampered)
	require.False(t, manager.Valid(req))

	// A different key must be rejected.
	other, err := NewSessionManager([]byte(strings.Repeat("z", 32)), time.Hour)
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.AddCookie(cookie)
	require.False(t, other.Valid(req))

	// No cookie is not a session.
	require.False(t, manager.Valid(httptest.NewRequest(http.MethodGet, "http://example.com/", nil)))

	// Expired tokens are rejected.
	manager.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	req = httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.AddCookie(cookie)
	require.False(t, manager.Valid(req))

	// Clearing removes the cookie.
	clearRecorder := httptest.NewRecorder()
	manager.Clear(clearRecorder, httptest.NewRequest(http.MethodPost, "http://example.com/", nil))
	cleared := clearRecorder.Result().Cookies()
	require.Len(t, cleared, 1)
	require.Equal(t, CookieName, cleared[0].Name)
	require.Equal(t, -1, cleared[0].MaxAge)
}

func TestSessionManagerSetsSecureCookieOnTLS(t *testing.T) {
	manager, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "https://example.com/", nil)
	req.TLS = &tls.ConnectionState{}
	recorder := httptest.NewRecorder()
	require.NoError(t, manager.Issue(recorder, req, "admin"))
	cookies := recorder.Result().Cookies()
	require.Len(t, cookies, 1)
	require.True(t, cookies[0].Secure)
}

func TestSessionManagerSetsSecureCookieBehindTLSProxy(t *testing.T) {
	manager, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	recorder := httptest.NewRecorder()
	require.NoError(t, manager.Issue(recorder, req, "admin"))
	cookies := recorder.Result().Cookies()
	require.Len(t, cookies, 1)
	require.True(t, cookies[0].Secure, "a proxied HTTPS request must get a Secure cookie")

	clearRecorder := httptest.NewRecorder()
	manager.Clear(clearRecorder, req)
	cleared := clearRecorder.Result().Cookies()
	require.Len(t, cleared, 1)
	require.True(t, cleared[0].Secure)
}

func TestSessionManagerRejectsEmptyUsername(t *testing.T) {
	manager, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)
	err = manager.Issue(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/", nil), "")
	require.Error(t, err)
}

func TestSessionManagerLogoutRevokesSession(t *testing.T) {
	manager, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)

	issueRecorder := httptest.NewRecorder()
	require.NoError(t, manager.Issue(issueRecorder, httptest.NewRequest(http.MethodGet, "http://example.com/", nil), "admin"))
	cookie := issueRecorder.Result().Cookies()[0]

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.AddCookie(cookie)
	require.True(t, manager.Valid(req))
	require.Equal(t, "admin", manager.Username(req))

	// Logout revokes the server-side session, not just the browser cookie.
	manager.Clear(httptest.NewRecorder(), req)

	reused := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	reused.AddCookie(cookie)
	require.False(t, manager.Valid(reused))
	require.Empty(t, manager.Username(reused))
}

func TestSessionManagerRestartInvalidatesSessions(t *testing.T) {
	manager, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)

	issueRecorder := httptest.NewRecorder()
	require.NoError(t, manager.Issue(issueRecorder, httptest.NewRequest(http.MethodGet, "http://example.com/", nil), "admin"))
	cookie := issueRecorder.Result().Cookies()[0]

	// The in-memory store is empty after a restart, so old cookies fail even
	// though the persisted signing key is reused.
	restarted, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.AddCookie(cookie)
	require.False(t, restarted.Valid(req))
}

func TestRequireAuth(t *testing.T) {
	manager, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := manager.RequireAuth(next)

	// Pages redirect to the login page.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://example.com/clients", nil))
	require.Equal(t, http.StatusSeeOther, recorder.Code)
	require.Equal(t, "/login", recorder.Header().Get("Location"))

	// API requests get a JSON 401.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://example.com/api/clients", nil))
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
	var payload map[string]string
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, "unauthorized", payload["error"])

	// JSON Accept is treated as an API request.
	req := httptest.NewRequest(http.MethodGet, "http://example.com/clients", nil)
	req.Header.Set("Accept", "application/json")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusUnauthorized, recorder.Code)

	// htmx requests get a redirect header so the browser can navigate.
	req = httptest.NewRequest(http.MethodGet, "http://example.com/clients/c1/detail", nil)
	req.Header.Set("HX-Request", "true")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.Equal(t, "/login", recorder.Header().Get("HX-Redirect"))

	// A valid session reaches the next handler.
	issueRecorder := httptest.NewRecorder()
	require.NoError(t, manager.Issue(issueRecorder, httptest.NewRequest(http.MethodGet, "http://example.com/", nil), "admin"))
	cookie := issueRecorder.Result().Cookies()[0]
	req = httptest.NewRequest(http.MethodGet, "http://example.com/clients", nil)
	req.AddCookie(cookie)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusNoContent, recorder.Code)
}

func TestRequireCSRF(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := RequireCSRF(next)

	newRequest := func(headers map[string]string) *http.Request {
		req := httptest.NewRequest(http.MethodPut, "http://example.com/api/clients/c1/mappings", nil)
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		return req
	}

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"missing header", nil, http.StatusForbidden},
		{"wrong header", map[string]string{"X-Requested-With": "fetch"}, http.StatusForbidden},
		{"evil origin", map[string]string{"X-Requested-With": "hy2tun", "Origin": "http://evil.example"}, http.StatusForbidden},
		{"null origin", map[string]string{"X-Requested-With": "hy2tun", "Origin": "null"}, http.StatusForbidden},
		{"matching origin", map[string]string{"X-Requested-With": "hy2tun", "Origin": "http://example.com"}, http.StatusNoContent},
		{"no origin", map[string]string{"X-Requested-With": "hy2tun"}, http.StatusForbidden},
		{"same-origin fetch metadata", map[string]string{"X-Requested-With": "hy2tun", "Origin": "http://example.com", "Sec-Fetch-Site": "same-origin"}, http.StatusNoContent},
		{"cross-site fetch metadata", map[string]string{"X-Requested-With": "hy2tun", "Origin": "http://example.com", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site fetch metadata", map[string]string{"X-Requested-With": "hy2tun", "Origin": "http://example.com", "Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, newRequest(tc.headers))
			require.Equal(t, tc.want, recorder.Code)
			if tc.want == http.StatusForbidden {
				require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
			}
		})
	}
}

func TestRequireCSRFRequiresMatchingScheme(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := RequireCSRF(next)

	cases := []struct {
		name   string
		target string
		origin string
	}{
		{"http request with https origin", "http://example.com/api/clients/c1/mappings", "https://example.com"},
		{"https request with http origin", "https://example.com/api/clients/c1/mappings", "http://example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, tc.target, nil)
			req.Header.Set("X-Requested-With", "hy2tun")
			req.Header.Set("Origin", tc.origin)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			require.Equal(t, http.StatusForbidden, recorder.Code)
		})
	}

	// A matching HTTPS origin is accepted.
	req := httptest.NewRequest(http.MethodPut, "https://example.com/api/clients/c1/mappings", nil)
	req.Header.Set("X-Requested-With", "hy2tun")
	req.Header.Set("Origin", "https://example.com")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusNoContent, recorder.Code)
}

func TestRequireCSRFUsesForwardedProtoBehindTLSProxy(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := RequireCSRF(next)

	newRequest := func(origin string) *http.Request {
		req := httptest.NewRequest(http.MethodPut, "http://example.com/api/clients/c1/mappings", nil)
		req.Header.Set("X-Requested-With", "hy2tun")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("Origin", origin)
		return req
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, newRequest("https://example.com"))
	require.Equal(t, http.StatusNoContent, recorder.Code)

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, newRequest("http://example.com"))
	require.Equal(t, http.StatusForbidden, recorder.Code)
}

func TestLoginHandlerSetTrustProxyHotToggle(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct"), bcrypt.MinCost)
	require.NoError(t, err)
	sessions, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)

	handler := &LoginHandler{
		Sessions:     sessions,
		Limiter:      NewLoginLimiter(1, time.Minute, time.Minute),
		Username:     "admin",
		PasswordHash: string(hash),
	}

	attempt := func(forwarded string) int {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/login", strings.NewReader("username=admin&password=wrong"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "10.0.0.5:1234"
		req.Header.Set("X-Forwarded-For", forwarded)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Code
	}

	// With trust disabled, every forwarded header shares the RemoteAddr bucket.
	require.Equal(t, http.StatusUnauthorized, attempt("203.0.113.1"))
	require.Equal(t, http.StatusTooManyRequests, attempt("203.0.113.2"))

	// Enabling trust immediately makes distinct forwarded clients independent.
	handler.SetTrustProxy(true)
	require.Equal(t, http.StatusUnauthorized, attempt("203.0.113.3"))
	require.Equal(t, http.StatusUnauthorized, attempt("203.0.113.4"))
	require.Equal(t, http.StatusTooManyRequests, attempt("203.0.113.3"))

	// Disabling it again returns to the shared RemoteAddr bucket.
	handler.SetTrustProxy(false)
	require.Equal(t, http.StatusTooManyRequests, attempt("203.0.113.5"))
}

func TestLoginHandlerTrustProxyRateLimit(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("correct"), bcrypt.MinCost)
	require.NoError(t, err)
	sessions, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)

	limiter := NewLoginLimiter(1, time.Minute, time.Minute)
	handler := &LoginHandler{
		Sessions:     sessions,
		Limiter:      limiter,
		Username:     "admin",
		PasswordHash: string(hash),
		TrustProxy:   true,
	}

	attempt := func(forwarded string) int {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/login", strings.NewReader("username=admin&password=wrong"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "10.0.0.5:1234"
		req.Header.Set("X-Forwarded-For", forwarded)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Code
	}

	require.Equal(t, http.StatusUnauthorized, attempt("203.0.113.1"))
	require.Equal(t, http.StatusTooManyRequests, attempt("203.0.113.1"))
	require.Equal(t, http.StatusUnauthorized, attempt("203.0.113.2"),
		"a different forwarded client must not share the first client's lockout")

	// Without trust, the same RemoteAddr shares one bucket regardless of the
	// forwarded header.
	untrustedLimiter := NewLoginLimiter(1, time.Minute, time.Minute)
	handler.Limiter = untrustedLimiter
	handler.TrustProxy = false
	require.Equal(t, http.StatusUnauthorized, attempt("203.0.113.3"))
	require.Equal(t, http.StatusTooManyRequests, attempt("203.0.113.4"))
}

func TestLoginLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	limiter := NewLoginLimiter(5, time.Minute, time.Minute)
	limiter.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		require.True(t, limiter.Allow("203.0.113.1"))
		limiter.Failure("203.0.113.1")
	}
	require.False(t, limiter.Allow("203.0.113.1"))
	require.True(t, limiter.Allow("203.0.113.2"))

	now = now.Add(61 * time.Second)
	require.True(t, limiter.Allow("203.0.113.1"))

	// A successful login clears the failure count.
	for i := 0; i < 4; i++ {
		limiter.Failure("203.0.113.1")
	}
	limiter.Success("203.0.113.1")
	for i := 0; i < 5; i++ {
		require.True(t, limiter.Allow("203.0.113.1"))
		limiter.Failure("203.0.113.1")
	}
	require.False(t, limiter.Allow("203.0.113.1"))

	// Failures spread over the window do not accumulate forever.
	for i := 0; i < 4; i++ {
		limiter.Failure("203.0.113.3")
	}
	now = now.Add(61 * time.Second)
	require.True(t, limiter.Allow("203.0.113.3"))
	limiter.Failure("203.0.113.3")
	require.True(t, limiter.Allow("203.0.113.3"))
}

func TestLoginHandler(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	require.NoError(t, err)
	sessions, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)
	limiter := NewLoginLimiter(5, time.Minute, time.Minute)
	handler := &LoginHandler{
		Sessions:     sessions,
		Limiter:      limiter,
		Username:     "admin",
		PasswordHash: string(hash),
	}

	loginRequest := func(username, password string) *http.Request {
		form := url.Values{"username": {username}, "password": {password}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/login", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, loginRequest("admin", "wrong"))
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	require.Contains(t, recorder.Body.String(), "invalid credentials")

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, loginRequest("admin", "pw"))
	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Len(t, recorder.Result().Cookies(), 1)

	// Missing fields are rejected.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, loginRequest("", ""))
	require.Equal(t, http.StatusUnauthorized, recorder.Code)

	// JSON credentials work too.
	recorder = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://example.com/api/login", strings.NewReader(`{"username":"admin","password":"pw"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusNoContent, recorder.Code)

	// Only POST is accepted.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "http://example.com/api/login", nil))
	require.Equal(t, http.StatusMethodNotAllowed, recorder.Code)
	require.Equal(t, "POST", recorder.Header().Get("Allow"))
}

func TestLoginHandlerRateLimit(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	require.NoError(t, err)
	sessions, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)
	handler := &LoginHandler{
		Sessions:     sessions,
		Limiter:      NewLoginLimiter(5, time.Minute, time.Minute),
		Username:     "admin",
		PasswordHash: string(hash),
	}

	loginRequest := func() *http.Request {
		form := url.Values{"username": {"admin"}, "password": {"wrong"}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/login", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return req
	}

	for i := 0; i < 5; i++ {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, loginRequest())
		require.Equal(t, http.StatusUnauthorized, recorder.Code)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, loginRequest())
	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.Equal(t, "60", recorder.Header().Get("Retry-After"))
}

func TestLoginHandlerRejectsOversizedBody(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	require.NoError(t, err)
	sessions, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)
	handler := &LoginHandler{
		Sessions:     sessions,
		Limiter:      NewLoginLimiter(5, time.Minute, time.Minute),
		Username:     "admin",
		PasswordHash: string(hash),
	}

	body := strings.Repeat("a", 5000)
	req := httptest.NewRequest(http.MethodPost, "http://example.com/api/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestClientIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	require.Equal(t, "203.0.113.9", ClientIP(req))

	req.RemoteAddr = "203.0.113.9"
	require.Equal(t, "203.0.113.9", ClientIP(req))
}

func TestClientIPWithProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.5")
	req.Header.Set("X-Real-IP", "203.0.113.8")

	// Default: forwarded headers are ignored.
	require.Equal(t, "10.0.0.5", ClientIPWithProxy(req, false))
	require.Equal(t, "10.0.0.5", ClientIP(req))

	// Trusted proxy: X-Real-IP is preferred.
	require.Equal(t, "203.0.113.8", ClientIPWithProxy(req, true))

	req.Header.Set("X-Real-IP", "")
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 203.0.113.6")
	require.Equal(t, "203.0.113.6", ClientIPWithProxy(req, true), "the last X-Forwarded-For entry is the fallback")

	req.Header.Set("X-Real-IP", "not-an-ip")
	req.Header.Set("X-Forwarded-For", "not-an-ip")
	require.Equal(t, "10.0.0.5", ClientIPWithProxy(req, true), "invalid forwarded values fall back to RemoteAddr")

	req.Header.Set("X-Forwarded-For", "[2001:db8::1]:4321")
	require.Equal(t, "2001:db8::1", ClientIPWithProxy(req, true))
}

func TestClientIPWithProxyIgnoresSpoofedFirstXFF(t *testing.T) {
	// A client can send its own X-Forwarded-For value, which an appending
	// proxy would place first. Only the last entry (the nearest proxy's
	// value) may key the limiter.
	first := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	first.RemoteAddr = "10.0.0.5:1234"
	first.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.7")

	second := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	second.RemoteAddr = "10.0.0.5:1234"
	second.Header.Set("X-Forwarded-For", "198.51.100.2, 203.0.113.7")

	require.Equal(t, "203.0.113.7", ClientIPWithProxy(first, true))
	require.Equal(t, ClientIPWithProxy(first, true), ClientIPWithProxy(second, true),
		"rotating the spoofed first entry must not change the limiter key")
}

func TestClientIPWithProxyPrefersXRealIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.66, 203.0.113.7")
	req.Header.Set("X-Real-IP", "203.0.113.8")

	require.Equal(t, "203.0.113.8", ClientIPWithProxy(req, true),
		"a spoofed X-Forwarded-For must not override the proxy's X-Real-IP")
}

func TestClientIPWithProxySingleXFF(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.9")

	require.Equal(t, "203.0.113.9", ClientIPWithProxy(req, true))
}

func TestClientIPWithProxyDefaultIgnoresForwardedHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.66")
	req.Header.Set("X-Real-IP", "203.0.113.8")

	require.Equal(t, "10.0.0.5", ClientIPWithProxy(req, false))
}

func TestUpdateAdminRevokesOldSessions(t *testing.T) {
	manager, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)

	issueRecorder := httptest.NewRecorder()
	require.NoError(t, manager.Issue(issueRecorder, httptest.NewRequest(http.MethodPost, "http://example.com/login", nil), "admin"))
	cookie := issueRecorder.Result().Cookies()[0]

	hash, err := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
	require.NoError(t, err)
	manager.UpdateAdmin("root", string(hash))
	manager.RevokeAll()

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.AddCookie(cookie)
	require.False(t, manager.Valid(req))
	require.True(t, manager.VerifyPassword("new-password"))
	require.False(t, manager.VerifyPassword("old-password"))
	require.Equal(t, "root", manager.AdminUsername())
}

func TestUpdateAdminRevokesSessionsWithoutExplicitRevoke(t *testing.T) {
	manager, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)

	issueRecorder := httptest.NewRecorder()
	require.NoError(t, manager.Issue(issueRecorder, httptest.NewRequest(http.MethodGet, "http://example.com/", nil), "admin"))
	cookie := issueRecorder.Result().Cookies()[0]

	hash, err := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
	require.NoError(t, err)
	manager.UpdateAdmin("root", string(hash))

	req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
	req.AddCookie(cookie)
	require.False(t, manager.Valid(req), "UpdateAdmin must revoke every existing session")
}

func TestSessionManagerIssueRejectsLoginVerifiedAgainstResetCredentials(t *testing.T) {
	manager, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)

	oldHash, err := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	require.NoError(t, err)
	manager.UpdateAdmin("admin", string(oldHash))

	username, verifiedHash, epoch, ok := manager.AdminWithEpoch()
	require.True(t, ok)
	require.NotZero(t, epoch)

	// A reset keeps the same username but replaces the hash. A login that
	// already copied the old hash must not create a session after this.
	newHash, err := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
	require.NoError(t, err)
	manager.UpdateAdmin(username, string(newHash))

	recorder := httptest.NewRecorder()
	err = manager.Issue(recorder, httptest.NewRequest(http.MethodPost, "http://example.com/login", nil), username,
		AdminCredential{PasswordHash: verifiedHash, Epoch: epoch})
	require.ErrorIs(t, err, ErrStaleAdmin)
	require.Empty(t, recorder.Result().Cookies())

	// The current snapshot can still issue a session.
	username, verifiedHash, epoch, ok = manager.AdminWithEpoch()
	require.True(t, ok)
	recorder = httptest.NewRecorder()
	require.NoError(t, manager.Issue(recorder, httptest.NewRequest(http.MethodPost, "http://example.com/login", nil), username,
		AdminCredential{PasswordHash: verifiedHash, Epoch: epoch}))
	require.Len(t, recorder.Result().Cookies(), 1)
}

func TestSessionManagerConcurrentLoginResetRejectsStaleSession(t *testing.T) {
	manager, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)

	oldHash, err := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	require.NoError(t, err)
	newHash, err := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
	require.NoError(t, err)

	for i := 0; i < 25; i++ {
		manager.UpdateAdmin("admin", string(oldHash))
		username, verifiedHash, epoch, ok := manager.AdminWithEpoch()
		require.True(t, ok)

		recorder := httptest.NewRecorder()
		var (
			wg       sync.WaitGroup
			issueErr error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			manager.UpdateAdmin(username, string(newHash))
		}()
		go func() {
			defer wg.Done()
			issueErr = manager.Issue(recorder, httptest.NewRequest(http.MethodPost, "http://example.com/login", nil), username,
				AdminCredential{PasswordHash: verifiedHash, Epoch: epoch})
		}()
		wg.Wait()

		if issueErr != nil {
			require.ErrorIs(t, issueErr, ErrStaleAdmin)
			require.Empty(t, recorder.Result().Cookies())
			continue
		}

		// The issue won the race but UpdateAdmin revoked every session, so the
		// cookie must no longer validate.
		cookies := recorder.Result().Cookies()
		require.Len(t, cookies, 1)
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		req.AddCookie(cookies[0])
		require.False(t, manager.Valid(req), "a session issued before the reset must be revoked")
	}
}

func TestLoginHandlerUsesUpdatedAdminCredentials(t *testing.T) {
	manager, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)
	firstHash, err := bcrypt.GenerateFromPassword([]byte("first-password"), bcrypt.MinCost)
	require.NoError(t, err)
	manager.UpdateAdmin("admin", string(firstHash))

	handler := &LoginHandler{Sessions: manager, Limiter: NewLoginLimiter(10, time.Minute, time.Minute)}

	attempt := func(username, password string) *httptest.ResponseRecorder {
		body := url.Values{"username": {username}, "password": {password}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	require.Equal(t, http.StatusNoContent, attempt("admin", "first-password").Code)

	secondHash, err := bcrypt.GenerateFromPassword([]byte("second-password"), bcrypt.MinCost)
	require.NoError(t, err)
	manager.UpdateAdmin("root", string(secondHash))

	require.Equal(t, http.StatusUnauthorized, attempt("admin", "first-password").Code)
	require.Equal(t, http.StatusUnauthorized, attempt("admin", "second-password").Code)
	require.Equal(t, http.StatusNoContent, attempt("root", "second-password").Code)
}

func TestLoginHandlerSetAdminUpdatesStaticFallback(t *testing.T) {
	sessions, err := NewSessionManager(testSecret(), time.Hour)
	require.NoError(t, err)
	oldHash, err := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	require.NoError(t, err)
	newHash, err := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
	require.NoError(t, err)

	handler := &LoginHandler{
		Sessions:     sessions,
		Username:     "admin",
		PasswordHash: string(oldHash),
	}
	attempt := func(username, password string) int {
		body := url.Values{"username": {username}, "password": {password}}.Encode()
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Code
	}

	require.Equal(t, http.StatusNoContent, attempt("admin", "old-password"))
	handler.SetAdmin("root", string(newHash))
	require.Equal(t, http.StatusUnauthorized, attempt("admin", "old-password"))
	require.Equal(t, http.StatusNoContent, attempt("root", "new-password"))
}
