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

// WebServer is the manager's HTTP surface. The authenticated shell is usable
// with a nil inventory; Task 7 wires the inventory during composition.
type WebServer struct {
	store     *Store
	audit     *AuditLog
	cfg       Config
	log       *slog.Logger
	base      string
	static    fs.FS
	inventory *Inventory

	sessions *SessionManager
	login    *LoginHandler

	loginTemplate    *template.Template
	overviewTemplate *template.Template
	serversTemplate  *template.Template
	serverTemplate   *template.Template
	clientsTemplate  *template.Template
	clientTemplate   *template.Template
	settingsTemplate *template.Template

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
	Servers             []ServerView
	InventoryConfigured bool
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
	serversTemplate, err := shared.ParsePage(managerAssets, "templates/layout.html", "templates/servers.html")
	if err != nil {
		return nil, fmt.Errorf("manager: parse servers template: %w", err)
	}
	serverTemplate, err := shared.ParsePage(managerAssets, "templates/layout.html", "templates/server.html")
	if err != nil {
		return nil, fmt.Errorf("manager: parse server template: %w", err)
	}
	clientsTemplate, err := shared.ParsePage(managerAssets, "templates/layout.html", "templates/clients.html")
	if err != nil {
		return nil, fmt.Errorf("manager: parse clients template: %w", err)
	}
	clientTemplate, err := shared.ParsePage(managerAssets, "templates/layout.html", "templates/client.html")
	if err != nil {
		return nil, fmt.Errorf("manager: parse client template: %w", err)
	}
	settingsTemplate, err := shared.ParsePage(managerAssets, "templates/layout.html", "templates/server_settings.html")
	if err != nil {
		return nil, fmt.Errorf("manager: parse server settings template: %w", err)
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
		serversTemplate:  serversTemplate,
		serverTemplate:   serverTemplate,
		clientsTemplate:  clientsTemplate,
		clientTemplate:   clientTemplate,
		settingsTemplate: settingsTemplate,
	}
	server.handler = server.routes()
	return server, nil
}

// Handler returns the manager HTTP handler, including the configured base
// path prefix and security headers.
func (s *WebServer) Handler() http.Handler {
	return s.handler
}

// SetInventory wires the server inventory into the shell. Task 7 calls it after
// constructing the inventory and before serving. A nil inventory keeps the
// shell usable and makes inventory APIs return 503.
func (s *WebServer) SetInventory(inventory *Inventory) {
	s.inventory = inventory
}

func (s *WebServer) routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /login", http.HandlerFunc(s.handleLoginPage))
	mux.Handle("POST /api/login", s.login)
	// Logout is protected by CSRF like every manager mutation. A missing or
	// invalid session is harmless: clearing it is a no-op.
	mux.Handle("POST /api/logout", shared.RequireCSRF(http.HandlerFunc(s.handleLogout)))

	mux.Handle("GET /{$}", s.requireAuth(http.HandlerFunc(s.handleOverviewPage)))

	// Inventory pages and JSON APIs. All mutations keep the Task 2 auth and
	// CSRF gates; destructive removal additionally requires the server name.
	mux.Handle("GET /servers", s.requireAuth(http.HandlerFunc(s.handleServersPage)))
	mux.Handle("GET /servers/{id}", s.requireAuth(http.HandlerFunc(s.handleServerPage)))
	mux.Handle("GET /servers/{id}/clients", s.requireAuth(http.HandlerFunc(s.handleClientsPage)))
	mux.Handle("GET /servers/{id}/clients/{clientID}", s.requireAuth(http.HandlerFunc(s.handleClientPage)))
	mux.Handle("GET /servers/{id}/settings", s.requireAuth(http.HandlerFunc(s.handleServerSettingsPage)))

	mux.Handle("GET /api/servers", s.requireAuth(http.HandlerFunc(s.handleListServersAPI)))
	mux.Handle("GET /api/servers/{id}", s.requireAuth(http.HandlerFunc(s.handleGetServerAPI)))
	mux.Handle("POST /api/servers", s.mutation(http.HandlerFunc(s.handleAddServer)))
	mux.Handle("PUT /api/servers/{id}", s.mutation(http.HandlerFunc(s.handleUpdateServer)))
	mux.Handle("DELETE /api/servers/{id}", s.mutation(http.HandlerFunc(s.handleRemoveServer)))
	mux.Handle("POST /api/servers/test", s.mutation(http.HandlerFunc(s.handleTestNewServer)))
	mux.Handle("POST /api/servers/refresh-all", s.mutation(http.HandlerFunc(s.handleRefreshAllServers)))
	mux.Handle("POST /api/servers/{id}/test", s.mutation(http.HandlerFunc(s.handleTestStoredServer)))
	mux.Handle("POST /api/servers/{id}/refresh", s.mutation(http.HandlerFunc(s.handleRefreshServer)))

	// Client mapping writes are full replacements authenticated by the
	// upstream revision; exposure is a separate server-side state mutation and
	// remains available while the client is offline.
	mux.Handle("PUT /api/servers/{id}/clients/{clientID}/mappings", s.mutation(http.HandlerFunc(s.handlePushClientMappings)))
	mux.Handle("PUT /api/servers/{id}/clients/{clientID}/mappings/{mappingID}/exposure", s.mutation(http.HandlerFunc(s.handleSetMappingExposure)))

	// Server settings and credential operations. The Web administrator update
	// uses the manager's stored current password and re-authenticates after
	// upstream acceptance. Revoke requires an explicit confirmation.
	mux.Handle("GET /api/servers/{id}/settings", s.requireAuth(http.HandlerFunc(s.handleGetServerSettingsAPI)))
	mux.Handle("PUT /api/servers/{id}/settings/mode", s.mutation(http.HandlerFunc(s.handleUpdateServerMode)))
	mux.Handle("PUT /api/servers/{id}/settings/ports", s.mutation(http.HandlerFunc(s.handleUpdateServerPorts)))
	mux.Handle("PUT /api/servers/{id}/settings/web", s.mutation(http.HandlerFunc(s.handleUpdateServerWebAdmin)))
	mux.Handle("PUT /api/servers/{id}/settings/trust-proxy", s.mutation(http.HandlerFunc(s.handleUpdateServerTrustProxy)))
	mux.Handle("PUT /api/servers/{id}/settings/credentials", s.mutation(http.HandlerFunc(s.handleRotateServerCredentials)))
	mux.Handle("POST /api/servers/{id}/settings/credentials/revoke", s.mutation(http.HandlerFunc(s.handleRevokeServerCredentials)))

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

func (s *WebServer) mutation(next http.Handler) http.Handler {
	return s.requireAuth(shared.RequireCSRF(next))
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
	data := overviewPageData{
		pageBase:            s.page("服务器清单", s.sessions.Username(r)),
		InventoryConfigured: s.inventory != nil,
	}
	if s.inventory != nil {
		data.Servers = s.inventory.List()
	}
	s.render(w, http.StatusOK, s.overviewTemplate, data)
}

func (s *WebServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Clear(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
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
