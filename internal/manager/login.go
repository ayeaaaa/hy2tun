package manager

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/webui/shared"
)

const (
	managerLoginBodyLimit  = 4 << 10
	managerLoginRetryAfter = "60"
)

// LoginHandler verifies the single manager administrator account and issues a
// manager session cookie. Passwords and session tokens are never logged or
// echoed in responses.
type LoginHandler struct {
	Sessions     *SessionManager
	Limiter      *shared.LoginLimiter
	Username     string
	PasswordHash string
	// TrustProxy makes the limiter key on X-Forwarded-For / X-Real-IP. Enable
	// it only behind a proxy that overwrites those headers.
	TrustProxy bool
	Log        *slog.Logger
}

// NewLoginHandler returns the manager login endpoint handler.
func NewLoginHandler(sessions *SessionManager, limiter *shared.LoginLimiter, username string, passwordHash string) *LoginHandler {
	return &LoginHandler{
		Sessions:     sessions,
		Limiter:      limiter,
		Username:     username,
		PasswordHash: passwordHash,
	}
}

// ServeHTTP implements http.Handler. It accepts form-encoded or JSON
// credentials and returns 204 on success.
func (h *LoginHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")

	ip := shared.ClientIPWithProxy(r, h.TrustProxy)
	if h.Limiter != nil && !h.Limiter.Allow(ip) {
		w.Header().Set("Retry-After", managerLoginRetryAfter)
		h.log().Debug("manager login rate limited", "ip", ip)
		writeJSONError(w, http.StatusTooManyRequests, "too many failed login attempts")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, managerLoginBodyLimit)
	username, password, err := loginCredentials(r)
	if err != nil {
		if h.Limiter != nil {
			h.Limiter.Failure(ip)
		}
		writeJSONError(w, http.StatusBadRequest, "invalid login request")
		return
	}
	if h.Sessions == nil || h.PasswordHash == "" {
		writeJSONError(w, http.StatusInternalServerError, "login is not configured")
		return
	}

	// Compare both fields in constant time and always run bcrypt so a failed
	// login does not reveal whether the username or the password was wrong.
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(h.Username)) == 1
	passOK := bcrypt.CompareHashAndPassword([]byte(h.PasswordHash), []byte(password)) == nil
	if !userOK || !passOK {
		if h.Limiter != nil {
			h.Limiter.Failure(ip)
		}
		// Never log the submitted username: users sometimes paste a password
		// into the wrong field. Only the source IP is safe to record.
		h.log().Debug("manager login failed", "ip", ip)
		writeJSONError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	if h.Limiter != nil {
		h.Limiter.Success(ip)
	}
	if err := h.Sessions.Issue(w, r, h.Username); err != nil {
		h.log().Error("issue manager session", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	h.log().Debug("manager login succeeded", "ip", ip, "username", h.Username)
	w.WriteHeader(http.StatusNoContent)
}

func (h *LoginHandler) log() *slog.Logger {
	if h != nil && h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func loginCredentials(r *http.Request) (string, string, error) {
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

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
