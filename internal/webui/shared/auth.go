// Package shared provides the authentication, CSRF protection, and template
// helpers used by the server and client web consoles.
package shared

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	// CookieName is the session cookie used by both web consoles.
	CookieName = "hy2tun_session"
	// CSRFHeader and CSRFValue are required on every authenticated mutation.
	CSRFHeader = "X-Requested-With"
	CSRFValue  = "hy2tun"

	defaultSessionTTL = 7 * 24 * time.Hour
	maxLoginBody      = 4 << 10

	defaultLoginFailures = 5
	defaultLoginWindow   = time.Minute
	defaultLoginLockout  = time.Minute
)

// ErrWeakSessionKey reports a session signing key shorter than 32 bytes.
var ErrWeakSessionKey = errors.New("shared: session key must be at least 32 bytes")

// SessionManager issues and validates HMAC-SHA256 signed session cookies.
// Issued session IDs are kept in memory so logout revokes a captured cookie
// immediately. Restarting the process invalidates all sessions.
type SessionManager struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time

	mu       sync.Mutex
	sessions map[string]time.Time
}

// NewSessionManager returns a manager using secret. The secret must be at
// least 32 bytes; it is copied so the caller may reuse the slice.
func NewSessionManager(secret []byte, ttl time.Duration) (*SessionManager, error) {
	if len(secret) < 32 {
		return nil, fmt.Errorf("%w (got %d bytes)", ErrWeakSessionKey, len(secret))
	}
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	key := make([]byte, len(secret))
	copy(key, secret)
	return &SessionManager{
		secret:   key,
		ttl:      ttl,
		now:      time.Now,
		sessions: make(map[string]time.Time),
	}, nil
}

type sessionClaims struct {
	Subject   string `json:"sub"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	SessionID string `json:"sid"`
}

// Issue signs a session cookie for username and writes it to w. The random
// session ID is recorded in memory so logout can revoke it.
func (m *SessionManager) Issue(w http.ResponseWriter, r *http.Request, username string) error {
	if m == nil {
		return errors.New("shared: session manager is nil")
	}
	if username == "" {
		return errors.New("shared: username is required")
	}

	now := m.now()
	var sessionIDBytes [16]byte
	if _, err := rand.Read(sessionIDBytes[:]); err != nil {
		return fmt.Errorf("shared: generate session ID: %w", err)
	}
	sessionID := hex.EncodeToString(sessionIDBytes[:])
	claims := sessionClaims{
		Subject:   username,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(m.ttl).Unix(),
		SessionID: sessionID,
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return fmt.Errorf("shared: encode session claims: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, m.secret)
	_, _ = mac.Write([]byte(encoded))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	m.mu.Lock()
	if m.sessions == nil {
		m.sessions = make(map[string]time.Time)
	}
	for existing, expiry := range m.sessions {
		if !now.Before(expiry) {
			delete(m.sessions, existing)
		}
	}
	m.sessions[sessionID] = now.Add(m.ttl)
	m.mu.Unlock()

	secure := RequestScheme(r) == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    encoded + "." + signature,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(m.ttl / time.Second),
		Expires:  now.Add(m.ttl),
	})
	return nil
}

// signedClaims verifies the cookie signature and its embedded timestamps
// without checking the server-side session store.
func (m *SessionManager) signedClaims(r *http.Request) (*sessionClaims, bool) {
	if m == nil || r == nil {
		return nil, false
	}
	cookie, err := r.Cookie(CookieName)
	if err != nil || cookie.Value == "" {
		return nil, false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 2 {
		return nil, false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, m.secret)
	_, _ = mac.Write([]byte(parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, false
	}
	var claims sessionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, false
	}
	now := m.now()
	if claims.Subject == "" ||
		claims.SessionID == "" ||
		claims.ExpiresAt <= now.Unix() ||
		claims.ExpiresAt <= claims.IssuedAt ||
		claims.IssuedAt > now.Add(time.Minute).Unix() {
		return nil, false
	}
	return &claims, true
}

func (m *SessionManager) claims(r *http.Request) (*sessionClaims, bool) {
	claims, ok := m.signedClaims(r)
	if !ok {
		return nil, false
	}
	now := m.now()
	m.mu.Lock()
	expiry, exists := m.sessions[claims.SessionID]
	if !exists || !now.Before(expiry) {
		delete(m.sessions, claims.SessionID)
		m.mu.Unlock()
		return nil, false
	}
	m.mu.Unlock()
	return claims, true
}

// Valid reports whether r carries a correctly signed, unexpired session.
func (m *SessionManager) Valid(r *http.Request) bool {
	_, ok := m.claims(r)
	return ok
}

// Username returns the subject of a valid session, or an empty string.
func (m *SessionManager) Username(r *http.Request) string {
	claims, ok := m.claims(r)
	if !ok {
		return ""
	}
	return claims.Subject
}

// Clear revokes the signed session in memory and writes an expired cookie.
// The same cookie value is rejected immediately even if it was captured.
func (m *SessionManager) Clear(w http.ResponseWriter, r *http.Request) {
	if claims, ok := m.signedClaims(r); ok {
		m.mu.Lock()
		delete(m.sessions, claims.SessionID)
		m.mu.Unlock()
	}
	secure := RequestScheme(r) == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}

// RequireAuth wraps next and requires a valid session. API and htmx requests
// receive 401 JSON; page requests are redirected to /login.
func (m *SessionManager) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if m.Valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		if IsAPIRequest(r) {
			if r.Header.Get("HX-Request") != "" {
				w.Header().Set("HX-Redirect", "/login")
			}
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// IsAPIRequest reports whether an unauthenticated request should receive a
// JSON 401 instead of a redirect. htmx fragments and JSON clients are treated
// as API requests.
func IsAPIRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		return true
	}
	if r.Header.Get("HX-Request") != "" {
		return true
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json")
}

// RequireCSRF rejects state-changing requests without X-Requested-With:
// hy2tun, without an Origin header, or with an Origin whose scheme or host
// differs from the request. Non-browser API clients must send an Origin header
// that matches the scheme and Host they use to reach the console.
func RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(CSRFHeader) != CSRFValue {
			writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		origin := r.Header.Get("Origin")
		if origin == "" {
			writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		parsed, err := url.Parse(origin)
		if err != nil ||
			parsed.Host == "" ||
			!strings.EqualFold(parsed.Scheme, RequestScheme(r)) ||
			!strings.EqualFold(parsed.Host, r.Host) {
			writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
		default:
			writeJSONError(w, http.StatusForbidden, "forbidden")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequestScheme returns the request's effective scheme. A TLS-terminating
// reverse proxy is a supported deployment, so a valid X-Forwarded-Proto is
// honored; the custom CSRF header and Host comparison remain the primary
// defenses for mutations.
func RequestScheme(r *http.Request) string {
	if r == nil {
		return ""
	}
	if r.TLS != nil {
		return "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		if first, _, ok := strings.Cut(forwarded, ","); ok {
			forwarded = first
		}
		switch scheme := strings.ToLower(strings.TrimSpace(forwarded)); scheme {
		case "http", "https":
			return scheme
		}
	}
	return "http"
}

type loginEntry struct {
	failures     int
	first        time.Time
	blockedUntil time.Time
}

// LoginLimiter is a small in-memory per-IP login failure limiter.
type LoginLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	lockout time.Duration
	now     func() time.Time
	entries map[string]*loginEntry
}

// NewLoginLimiter creates a limiter allowing max failures per window before
// locking the IP for lockout. Zero or negative values use the documented
// defaults (5 failures, 1 minute, 1 minute).
func NewLoginLimiter(max int, window, lockout time.Duration) *LoginLimiter {
	if max <= 0 {
		max = defaultLoginFailures
	}
	if window <= 0 {
		window = defaultLoginWindow
	}
	if lockout <= 0 {
		lockout = defaultLoginLockout
	}
	return &LoginLimiter{
		max:     max,
		window:  window,
		lockout: lockout,
		now:     time.Now,
		entries: make(map[string]*loginEntry),
	}
}

// Allow reports whether ip may attempt a login now.
func (l *LoginLimiter) Allow(ip string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.pruneLocked(now)
	entry := l.entries[ip]
	if entry == nil {
		return true
	}
	if now.Before(entry.blockedUntil) {
		return false
	}
	return true
}

// Failure records one failed login for ip.
func (l *LoginLimiter) Failure(ip string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	entry := l.entries[ip]
	if entry != nil && now.Before(entry.blockedUntil) {
		return
	}
	if entry != nil && !entry.blockedUntil.IsZero() && !now.Before(entry.blockedUntil) {
		entry = nil
	}
	if entry == nil || (!entry.first.IsZero() && now.Sub(entry.first) >= l.window) {
		entry = &loginEntry{first: now}
		l.entries[ip] = entry
	}
	entry.failures++
	if entry.failures >= l.max {
		entry.failures = 0
		entry.first = time.Time{}
		entry.blockedUntil = now.Add(l.lockout)
	}
	l.pruneLocked(now)
}

// Success clears the failure record for ip.
func (l *LoginLimiter) Success(ip string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	delete(l.entries, ip)
	l.mu.Unlock()
}

func (l *LoginLimiter) pruneLocked(now time.Time) {
	for ip, entry := range l.entries {
		if now.Before(entry.blockedUntil) {
			continue
		}
		if entry.first.IsZero() || now.Sub(entry.first) >= l.window {
			delete(l.entries, ip)
		}
	}
}

// LoginSuccessFunc runs after a session cookie has been issued.
type LoginSuccessFunc func(w http.ResponseWriter, r *http.Request)

// LoginErrorFunc renders or encodes a failed login response.
type LoginErrorFunc func(w http.ResponseWriter, r *http.Request, status int, message string)

// LoginHandler verifies the single administrator account and issues a session
// cookie. Passwords and session tokens are never logged.
type LoginHandler struct {
	Sessions     *SessionManager
	Limiter      *LoginLimiter
	Username     string
	PasswordHash string
	Log          *slog.Logger
	// TrustProxy makes the limiter key on X-Forwarded-For / X-Real-IP. Enable
	// it only when a trusted reverse proxy overwrites those headers.
	TrustProxy bool
	OnSuccess  LoginSuccessFunc
	OnError    LoginErrorFunc
}

// ServeHTTP implements http.Handler.
func (h *LoginHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.fail(w, r, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")

	ip := ClientIPWithProxy(r, h.TrustProxy)
	if h.Limiter != nil && !h.Limiter.Allow(ip) {
		w.Header().Set("Retry-After", "60")
		h.log().Debug("web login rate limited", "ip", ip)
		h.fail(w, r, http.StatusTooManyRequests, "too many failed login attempts")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)
	username, password, err := h.credentials(r)
	if err != nil {
		if h.Limiter != nil {
			h.Limiter.Failure(ip)
		}
		h.fail(w, r, http.StatusBadRequest, "invalid login request")
		return
	}
	if h.Sessions == nil || h.PasswordHash == "" {
		h.fail(w, r, http.StatusInternalServerError, "login is not configured")
		return
	}

	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(h.Username)) == 1
	passOK := bcrypt.CompareHashAndPassword([]byte(h.PasswordHash), []byte(password)) == nil
	if !userOK || !passOK {
		if h.Limiter != nil {
			h.Limiter.Failure(ip)
		}
		// Never log the submitted username: users sometimes paste a password
		// into the wrong field. Only the source IP is safe to record.
		h.log().Debug("web login failed", "ip", ip)
		h.fail(w, r, http.StatusUnauthorized, "invalid credentials")
		return
	}

	if h.Limiter != nil {
		h.Limiter.Success(ip)
	}
	if err := h.Sessions.Issue(w, r, h.Username); err != nil {
		h.log().Error("issue web session", "error", err)
		h.fail(w, r, http.StatusInternalServerError, "could not create session")
		return
	}
	h.log().Debug("web login succeeded", "ip", ip, "username", h.Username)
	if h.OnSuccess != nil {
		h.OnSuccess(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *LoginHandler) credentials(r *http.Request) (string, string, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var payload struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&payload); err != nil {
			return "", "", err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return "", "", errors.New("trailing data after JSON object")
		}
		return payload.Username, payload.Password, nil
	}
	if err := r.ParseForm(); err != nil {
		return "", "", err
	}
	return r.PostForm.Get("username"), r.PostForm.Get("password"), nil
}

func (h *LoginHandler) fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	if h.OnError != nil {
		h.OnError(w, r, status, message)
		return
	}
	writeJSONError(w, status, message)
}

func (h *LoginHandler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

// ClientIP returns the request's remote host without its port. Forwarded
// headers are deliberately ignored because they are attacker-controlled unless
// a trusted proxy strips them.
func ClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// ClientIPWithProxy returns the key used for login rate limiting. Forwarded
// headers are consulted only when trustProxy is enabled; without a trusted
// proxy they are attacker-controlled and would let a client evade the limiter
// by forging a new IP per request. With a trusted proxy, the first
// X-Forwarded-For entry is the original client.
func ClientIPWithProxy(r *http.Request, trustProxy bool) string {
	if r == nil {
		return ""
	}
	if trustProxy {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			first, _, _ := strings.Cut(forwarded, ",")
			if ip := normalizeClientIP(first); ip != "" {
				return ip
			}
		}
		if ip := normalizeClientIP(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
	}
	return ClientIP(r)
}

// normalizeClientIP accepts a bare IP, a bracketed IPv6 address, or host:port
// and returns the canonical host, or an empty string when it is not an IP.
func normalizeClientIP(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	value = strings.Trim(value, "[]")
	if net.ParseIP(value) == nil {
		return ""
	}
	return value
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
