package manager

import (
	"bytes"
	"crypto/rand"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/webui/shared"
)

// managerAssets holds the manager-only templates and static files. It is
// deliberately separate from the native server console assets.
//
//go:embed templates/*.html static/*
var managerAssets embed.FS

// WebServer is the manager's HTTP surface. Task 2 provides the authenticated
// shell; later tasks add inventory and upstream operations.
type WebServer struct {
	store  *Store
	audit  *AuditLog
	cfg    Config
	log    *slog.Logger
	base   string
	static fs.FS

	sessions *SessionManager
	login    *LoginHandler

	loginTemplate    *template.Template
	overviewTemplate *template.Template

	handler http.Handler
}

type pageBase struct {
	Title      string
	BasePath   string
	Username   string
	CSRFHeader string
	CSRFValue  string
}

type loginPageData struct {
	pageBase
}

type overviewPageData struct {
	pageBase
}

// NewWebServer builds the manager HTTP handler. The store already contains the
// single administrator record; the session signing key is fresh per process so
// a restart invalidates all manager cookies.
func NewWebServer(store *Store, audit *AuditLog, cfg Config, log *slog.Logger) (*WebServer, error) {
	if store == nil {
		return nil, fmt.Errorf("manager: store is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	admin := store.State().Admin
	if strings.TrimSpace(admin.Username) == "" {
		return nil, fmt.Errorf("manager: administrator username is empty")
	}
	if _, err := bcrypt.Cost([]byte(admin.PasswordHash)); err != nil {
		return nil, fmt.Errorf("manager: administrator password hash is invalid: %w", err)
	}

	basePath := normalizeBasePath(cfg.BasePath)
	cfg.BasePath = basePath
	secret := make([]byte, minManagerSessionKeySize)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("manager: generate session signing key: %w", err)
	}
	sessions, err := NewSessionManager(secret, defaultManagerSessionTTL)
	if err != nil {
		return nil, err
	}

	login := NewLoginHandler(sessions, shared.NewLoginLimiter(5, time.Minute, time.Minute), admin.Username, admin.PasswordHash)
	login.TrustProxy = cfg.TrustProxy
	login.Log = log

	loginTemplate, err := shared.ParsePage(managerAssets, "templates/layout.html", "templates/login.html")
	if err != nil {
		return nil, fmt.Errorf("manager: parse login template: %w", err)
	}
	overviewTemplate, err := shared.ParsePage(managerAssets, "templates/layout.html", "templates/overview.html")
	if err != nil {
		return nil, fmt.Errorf("manager: parse overview template: %w", err)
	}
	staticFS, err := fs.Sub(managerAssets, "static")
	if err != nil {
		return nil, fmt.Errorf("manager: open static assets: %w", err)
	}

	server := &WebServer{
		store:            store,
		audit:            audit,
		cfg:              cfg,
		log:              log,
		base:             basePath,
		static:           staticFS,
		sessions:         sessions,
		login:            login,
		loginTemplate:    loginTemplate,
		overviewTemplate: overviewTemplate,
	}
	server.handler = server.routes()
	return server, nil
}

// Handler returns the manager HTTP handler, including the configured base
// path prefix and security headers.
func (s *WebServer) Handler() http.Handler {
	return s.handler
}

func (s *WebServer) routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /login", http.HandlerFunc(s.handleLoginPage))
	mux.Handle("POST /api/login", s.login)
	// Logout is protected by CSRF like every manager mutation. A missing or
	// invalid session is harmless: clearing it is a no-op.
	mux.Handle("POST /api/logout", shared.RequireCSRF(http.HandlerFunc(s.handleLogout)))

	mux.Handle("GET /{$}", s.requireAuth(http.HandlerFunc(s.handleOverviewPage)))

	// Task 4 replaces this placeholder with inventory CRUD. The route exists
	// now so authenticated mutation requests reach the shared CSRF gate.
	mux.Handle("POST /api/servers", s.requireAuth(shared.RequireCSRF(http.HandlerFunc(s.handleServersPlaceholder))))

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(s.static))))

	var handler http.Handler = mux
	if s.base != "/" {
		prefix := strings.TrimSuffix(s.base, "/")
		stripped := http.StripPrefix(prefix, mux)
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == prefix {
				target := s.base
				if r.URL.RawQuery != "" {
					target += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, target, http.StatusTemporaryRedirect)
				return
			}
			stripped.ServeHTTP(w, r)
		})
	}
	return s.withSecurityHeaders(handler)
}

func (s *WebServer) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if s.sessions.Valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		if shared.IsAPIRequest(r) {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		http.Redirect(w, r, s.base+"login", http.StatusSeeOther)
	})
}

func (s *WebServer) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
				"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if shared.RequestScheme(r) == "https" {
			header.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *WebServer) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.sessions.Valid(r) {
		http.Redirect(w, r, s.base, http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, s.loginTemplate, loginPageData{pageBase: s.page("登录", "")})
}

func (s *WebServer) handleOverviewPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, s.overviewTemplate, overviewPageData{
		pageBase: s.page("服务器清单", s.sessions.Username(r)),
	})
}

func (s *WebServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Clear(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *WebServer) handleServersPlaceholder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotImplemented)
	_, _ = w.Write([]byte(`{"error":"not implemented","code":"not_implemented"}` + "\n"))
}

func (s *WebServer) page(title, username string) pageBase {
	return pageBase{
		Title:      title,
		BasePath:   s.base,
		Username:   username,
		CSRFHeader: shared.CSRFHeader,
		CSRFValue:  shared.CSRFValue,
	}
}

func (s *WebServer) render(w http.ResponseWriter, status int, tmpl *template.Template, data any) {
	var rendered bytes.Buffer
	if err := tmpl.ExecuteTemplate(&rendered, "layout", data); err != nil {
		s.log.Error("render manager page", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(rendered.Bytes())
}
