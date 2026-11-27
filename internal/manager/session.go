package manager

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"hy2tun/internal/webui/shared"
)

const (
	// managerSessionCookieName is deliberately distinct from the native
	// server console's session cookie so both can share a hostname.
	managerSessionCookieName = "hy2tun_manager_session"
	defaultManagerSessionTTL = 12 * time.Hour
	minManagerSessionKeySize = 32
)

// ErrWeakSessionKey reports a manager session signing key shorter than the
// minimum HMAC key size.
var ErrWeakSessionKey = errors.New("manager: session key must be at least 32 bytes")

// SessionManager issues and validates manager administrator session cookies.
// Cookies are HMAC-SHA256 signed and carry a random session ID plus the
// administrator username. Logout records the session ID in an in-memory
// revocation set until the signed expiry, so a captured cookie cannot be
// replayed. The set is not persisted: a process restart invalidates sessions.
type SessionManager struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	revoked map[string]time.Time
}

type managerSessionClaims struct {
	SessionID string `json:"sid"`
	Subject   string `json:"sub"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// NewSessionManager returns a manager using secret. The secret must be at
// least 32 bytes; it is copied so the caller may reuse the slice. A
// non-positive ttl uses the manager default.
func NewSessionManager(secret []byte, ttl time.Duration) (*SessionManager, error) {
	if len(secret) < minManagerSessionKeySize {
		return nil, fmt.Errorf("%w (got %d bytes)", ErrWeakSessionKey, len(secret))
	}
	if ttl <= 0 {
		ttl = defaultManagerSessionTTL
	}
	key := make([]byte, len(secret))
	copy(key, secret)
	return &SessionManager{
		secret:  key,
		ttl:     ttl,
		now:     time.Now,
		revoked: make(map[string]time.Time),
	}, nil
}

// Issue signs a session cookie for username and writes it to w. The token is
// base64url(claims).base64url(HMAC-SHA256(claims)); the claims contain the
// random session ID and username and are authenticated as a unit.
func (m *SessionManager) Issue(w http.ResponseWriter, r *http.Request, username string) error {
	if m == nil {
		return errors.New("manager: session manager is nil")
	}
	if username == "" {
		return errors.New("manager: session username is required")
	}

	now := m.now()
	var sessionIDBytes [16]byte
	if _, err := rand.Read(sessionIDBytes[:]); err != nil {
		return fmt.Errorf("manager: generate session ID: %w", err)
	}
	claims := managerSessionClaims{
		SessionID: base64.RawURLEncoding.EncodeToString(sessionIDBytes[:]),
		Subject:   username,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(m.ttl).Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return fmt.Errorf("manager: encode session claims: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	token := encoded + "." + m.sign(encoded)

	http.SetCookie(w, &http.Cookie{
		Name:     managerSessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   shared.RequestScheme(r) == "https",
		MaxAge:   int(m.ttl / time.Second),
		Expires:  now.Add(m.ttl),
	})
	return nil
}

// Valid reports whether r carries a correctly signed, unexpired, and
// non-revoked manager session.
func (m *SessionManager) Valid(r *http.Request) bool {
	_, ok := m.claims(r)
	return ok
}

// Username returns the subject of a valid manager session, or an empty
// string.
func (m *SessionManager) Username(r *http.Request) string {
	claims, ok := m.claims(r)
	if !ok {
		return ""
	}
	return claims.Subject
}

// Clear revokes the request's session in memory and writes an expired cookie.
// The same cookie value is rejected immediately even if it was captured.
func (m *SessionManager) Clear(w http.ResponseWriter, r *http.Request) {
	if m != nil && r != nil {
		if claims, ok := m.signedClaims(r); ok {
			now := m.now()
			m.mu.Lock()
			m.pruneLocked(now)
			m.revoked[claims.SessionID] = time.Unix(claims.ExpiresAt, 0)
			m.mu.Unlock()
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     managerSessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   shared.RequestScheme(r) == "https",
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})
}

func (m *SessionManager) claims(r *http.Request) (*managerSessionClaims, bool) {
	claims, ok := m.signedClaims(r)
	if !ok {
		return nil, false
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	if _, revoked := m.revoked[claims.SessionID]; revoked {
		return nil, false
	}
	return claims, true
}

// signedClaims verifies the cookie signature and timestamps without consulting
// the server-side revocation set.
func (m *SessionManager) signedClaims(r *http.Request) (*managerSessionClaims, bool) {
	if m == nil || r == nil {
		return nil, false
	}
	cookie, err := r.Cookie(managerSessionCookieName)
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
	var claims managerSessionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, false
	}
	now := m.now()
	if claims.SessionID == "" ||
		claims.Subject == "" ||
		claims.ExpiresAt <= now.Unix() ||
		claims.ExpiresAt <= claims.IssuedAt ||
		claims.IssuedAt > now.Add(time.Minute).Unix() {
		return nil, false
	}
	return &claims, true
}

func (m *SessionManager) sign(encoded string) string {
	mac := hmac.New(sha256.New, m.secret)
	_, _ = mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (m *SessionManager) pruneLocked(now time.Time) {
	for sessionID, expiry := range m.revoked {
		if !now.Before(expiry) {
			delete(m.revoked, sessionID)
		}
	}
}
