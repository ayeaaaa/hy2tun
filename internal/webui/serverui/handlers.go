// Package serverui implements the hy2tund web console: an authenticated,
// server-rendered overview of clients, mappings, traffic, and configuration.
package serverui

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/config"
	"hy2tun/internal/credentials"
	"hy2tun/internal/view"
	"hy2tun/internal/webui/shared"
)

// API is the subset of the server used by the web console.
type API interface {
	Overview() view.Overview
	Clients() []view.ClientView
	Client(id string) (view.ClientView, bool)
	DeleteClient(id string) error
	PushMappings(id string, baseRevision int64, mappings []config.Mapping) (int64, error)
	Settings() SettingsView
	UpdateWebConfig(username, passwordHash string) error
	UpdatePorts(allow, deny []string, autoRange string, offlineReserve time.Duration) error
	RotateCredentials(auth, controlToken, syncMode string) (int64, []view.CredentialResult, error)
	RevokePreviousCredentials() error
	CredentialsSnapshot() *credentials.Set
	SetTrustProxy(bool) error
}

// ServerAPI is an alias for API, kept for callers that use the descriptive
// name from the design documents.
type ServerAPI = API

// Server is an authenticated HTTP server for the hy2tund web console.
type Server struct {
	api API
	cfg config.WebConfig
	log *slog.Logger

	sessions        *shared.SessionManager
	limiter         *shared.LoginLimiter
	settingsLimiter *shared.LoginLimiter
	login           *shared.LoginHandler
	trustProxy      atomic.Bool

	pages    map[string]*template.Template
	handler  http.Handler
	staticFS fs.FS

	eventInterval time.Duration
	done          chan struct{}

	adminMu    sync.RWMutex
	webAdminMu sync.Mutex
	mu         sync.Mutex
	started    bool
	closed     bool
	listener   net.Listener
	httpServer *http.Server

	closeOnce sync.Once
	closeErr  error
}

// New validates cfg, parses the embedded templates, and returns a web console
// that has not started listening yet.
func New(api API, cfg config.WebConfig, sessionKey []byte, log *slog.Logger) (*Server, error) {
	if api == nil {
		return nil, fmt.Errorf("serverui: API is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.Username == "" {
		cfg.Username = "admin"
	}
	if cfg.PasswordHash == "" {
		return nil, fmt.Errorf("serverui: web password hash is required")
	}
	if _, err := bcrypt.Cost([]byte(cfg.PasswordHash)); err != nil {
		return nil, fmt.Errorf("serverui: web password hash is invalid: %w", err)
	}
	if (cfg.TLS.Cert == "") != (cfg.TLS.Key == "") {
		return nil, fmt.Errorf("serverui: web TLS cert and key must be set together")
	}

	sessions, err := shared.NewSessionManager(sessionKey, 7*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("serverui: %w", err)
	}
	// The session manager owns the live administrator credentials. The login
	// handler reads them on every attempt so a hot web-admin update applies
	// without restarting the console.
	sessions.UpdateAdmin(cfg.Username, cfg.PasswordHash)
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("serverui: static assets: %w", err)
	}

	s := &Server{
		api:             api,
		cfg:             cfg,
		log:             log,
		sessions:        sessions,
		limiter:         shared.NewLoginLimiter(5, time.Minute, time.Minute),
		settingsLimiter: shared.NewLoginLimiter(5, time.Minute, time.Minute),
		staticFS:        staticFS,
		eventInterval:   2 * time.Second,
		done:            make(chan struct{}),
	}
	s.login = &shared.LoginHandler{
		Sessions:     sessions,
		Limiter:      s.limiter,
		Username:     cfg.Username,
		PasswordHash: cfg.PasswordHash,
		Log:          log,
		TrustProxy:   cfg.TrustProxy,
		OnSuccess: func(w http.ResponseWriter, r *http.Request) {
			if wantsHTML(r) {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		},
		OnError: func(w http.ResponseWriter, r *http.Request, status int, message string) {
			if wantsHTML(r) {
				s.render(w, status, "login", loginPage{Username: s.sessions.AdminUsername(), Error: message})
				return
			}
			writeJSONErrorCode(w, status, "", message)
		},
	}
	s.trustProxy.Store(cfg.TrustProxy)

	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	s.handler = s.newHandler()
	return s, nil
}

// NewServer is an alias for New, kept for callers that prefer an explicit
// constructor name.
func NewServer(api API, cfg config.WebConfig, sessionKey []byte, log *slog.Logger) (*Server, error) {
	return New(api, cfg, sessionKey, log)
}

// Handler returns the HTTP handler. It is usable without Start for tests or
// for embedding behind another listener.
func (s *Server) Handler() http.Handler {
	return s.handler
}

// Addr returns the bound web address, or the configured address before Start.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.cfg.Listen
}

// SetTrustProxy updates whether the login and settings-confirmation rate
// limiters trust forwarded client IP headers. It takes effect for subsequent requests without
// a restart.
func (s *Server) SetTrustProxy(trust bool) {
	if s == nil {
		return
	}
	s.trustProxy.Store(trust)
	if s.login != nil {
		s.login.SetTrustProxy(trust)
	}
}

// SetAdmin replaces the live web administrator credentials and revokes every
// existing session. It also keeps the console's config copy and the login
// handler's static fallback in sync. The caller must validate and hash the
// password first.
func (s *Server) SetAdmin(username, passwordHash string) {
	if s == nil || s.sessions == nil {
		return
	}
	s.adminMu.Lock()
	s.cfg.Username = username
	s.cfg.PasswordHash = passwordHash
	s.adminMu.Unlock()
	s.sessions.UpdateAdmin(username, passwordHash)
	s.sessions.RevokeAll()
	if s.login != nil {
		s.login.SetAdmin(username, passwordHash)
	}
}

// Start binds the web listener and serves in the background. It returns once
// the listener is bound, so bind and certificate errors are reported
// synchronously. HTTPS is used when web.tls.cert/key are configured.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("serverui: web server is closed")
	}
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("serverui: web server already started")
	}
	if strings.TrimSpace(s.cfg.Listen) == "" {
		s.mu.Unlock()
		return fmt.Errorf("serverui: web.listen is empty")
	}
	s.started = true
	s.mu.Unlock()

	fail := func(err error) error {
		s.mu.Lock()
		s.started = false
		s.mu.Unlock()
		return err
	}

	var tlsConfig *tls.Config
	if s.cfg.TLS.Cert != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLS.Cert, s.cfg.TLS.Key)
		if err != nil {
			return fail(fmt.Errorf("serverui: load web TLS certificate: %w", err))
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	listener, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fail(fmt.Errorf("serverui: listen web %s: %w", s.cfg.Listen, err))
	}

	httpServer := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		TLSConfig:         tlsConfig,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = listener.Close()
		return fail(fmt.Errorf("serverui: web server is closed"))
	}
	s.listener = listener
	s.httpServer = httpServer
	s.mu.Unlock()

	scheme := "http"
	if tlsConfig != nil {
		scheme = "https"
	}
	s.log.Info("web console listening", "address", listener.Addr().String(), "scheme", scheme)

	go func() {
		select {
		case <-s.done:
			_ = listener.Close()
			return
		default:
		}
		var serveErr error
		if tlsConfig != nil {
			serveErr = httpServer.ServeTLS(listener, "", "")
		} else {
			serveErr = httpServer.Serve(listener)
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			s.log.Error("web console stopped", "error", serveErr)
		}
	}()
	return nil
}

// Close stops the web server. It is idempotent.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		httpServer := s.httpServer
		s.mu.Unlock()
		close(s.done)

		if httpServer == nil {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			_ = httpServer.Close()
			s.closeErr = err
		}
	})
	return s.closeErr
}

func (s *Server) newHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.Handle("POST /api/login", s.login)
	mux.Handle("POST /api/logout", shared.RequireCSRF(http.HandlerFunc(s.handleLogout)))

	mux.Handle("GET /{$}", s.auth(http.HandlerFunc(s.handleOverviewPage)))
	mux.Handle("GET /clients", s.auth(http.HandlerFunc(s.handleClientsPage)))
	mux.Handle("GET /clients/{id}/detail", s.auth(http.HandlerFunc(s.handleClientDetail)))
	mux.Handle("GET /mappings", s.auth(http.HandlerFunc(s.handleMappingsPage)))
	mux.Handle("GET /settings", s.auth(http.HandlerFunc(s.handleSettingsPage)))

	mux.Handle("GET /api/overview", s.auth(http.HandlerFunc(s.handleOverviewJSON)))
	mux.Handle("GET /api/clients", s.auth(http.HandlerFunc(s.handleClientsJSON)))
	mux.Handle("GET /api/clients/{id}", s.auth(http.HandlerFunc(s.handleClientJSON)))
	mux.Handle("DELETE /api/clients/{id}", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleDeleteClient))))
	mux.Handle("PUT /api/clients/{id}/mappings", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handlePushMappings))))
	mux.Handle("GET /api/mappings", s.auth(http.HandlerFunc(s.handleMappingsJSON)))
	mux.Handle("GET /api/events", s.auth(http.HandlerFunc(s.handleEvents)))
	mux.Handle("GET /api/config/summary", s.auth(http.HandlerFunc(s.handleConfigSummaryJSON)))

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	mux.Handle("GET /api/settings", s.auth(http.HandlerFunc(s.handleSettingsJSON)))
	mux.Handle("PUT /api/settings/web", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleUpdateWebSettings))))
	mux.Handle("PUT /api/settings/ports", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleUpdatePorts))))
	mux.Handle("PUT /api/settings/proxy", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleUpdateTrustProxy))))
	mux.Handle("PUT /api/settings/credentials", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleRotateCredentials))))
	mux.Handle("POST /api/settings/credentials/revoke", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleRevokeCredentials))))
	mux.Handle("POST /api/settings/credentials/reveal", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleRevealCredentials))))

	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServer(http.FS(s.staticFS)))))

	return s.withSecurityHeaders(mux)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return s.sessions.RequireAuth(next)
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
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

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.sessions.Valid(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	message := ""
	switch r.URL.Query().Get("error") {
	case "rate":
		message = "尝试次数过多，请稍后再试。"
	case "invalid":
		message = "用户名或密码错误。"
	}
	s.render(w, http.StatusOK, "login", loginPage{Username: s.sessions.AdminUsername(), Error: message})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Clear(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleOverviewPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "overview", overviewPage{
		pageBase: s.base("概览", "overview"),
		Overview: s.api.Overview(),
		Clients:  s.api.Clients(),
		Summary:  s.configSummary(),
	})
}

func (s *Server) handleClientsPage(w http.ResponseWriter, r *http.Request) {
	clients := s.api.Clients()
	online := 0
	for _, client := range clients {
		if client.Online {
			online++
		}
	}

	selectedID := r.URL.Query().Get("id")
	var selected *view.ClientView
	var detail *clientDetailData
	status := http.StatusOK
	notice := ""
	if selectedID != "" {
		found := false
		for i := range clients {
			if clients[i].ID != selectedID {
				continue
			}
			client := clients[i]
			selected = &client
			data := clientDetailDataFrom(client)
			detail = &data
			found = true
			break
		}
		if !found {
			status = http.StatusNotFound
			notice = "客户端不存在或已被移除。"
		}
	}

	s.render(w, status, "clients", clientsPage{
		pageBase:    s.base("客户端", "clients"),
		Clients:     clients,
		Selected:    selected,
		Detail:      detail,
		OnlineCount: online,
		Notice:      notice,
	})
}

func (s *Server) handleClientDetail(w http.ResponseWriter, r *http.Request) {
	client, ok := s.api.Client(r.PathValue("id"))
	if !ok {
		http.Error(w, "client not found", http.StatusNotFound)
		return
	}
	s.render(w, http.StatusOK, "client_detail", clientDetailDataFrom(client))
}

func (s *Server) handleMappingsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "mappings", mappingsPage{
		pageBase: s.base("全部映射", "mappings"),
		Rows:     mappingRows(s.api.Clients()),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	overview := s.api.Overview()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, `# HELP hy2tun_server_up Whether the server process is serving this request.
# TYPE hy2tun_server_up gauge
hy2tun_server_up 1
# HELP hy2tun_server_online_clients Number of currently online hy2tun clients.
# TYPE hy2tun_server_online_clients gauge
hy2tun_server_online_clients %d
# HELP hy2tun_server_active_mappings Number of active reverse mappings.
# TYPE hy2tun_server_active_mappings gauge
hy2tun_server_active_mappings %d
# HELP hy2tun_server_tx_bytes_total Total bytes sent to clients.
# TYPE hy2tun_server_tx_bytes_total counter
hy2tun_server_tx_bytes_total %d
# HELP hy2tun_server_rx_bytes_total Total bytes received from clients.
# TYPE hy2tun_server_rx_bytes_total counter
hy2tun_server_rx_bytes_total %d
# HELP hy2tun_server_uptime_seconds Server process uptime in seconds.
# TYPE hy2tun_server_uptime_seconds gauge
hy2tun_server_uptime_seconds %.0f
`, overview.OnlineClients, overview.ActiveMappings, overview.TxBytes, overview.RxBytes, overview.Uptime.Seconds())
}

func (s *Server) handleSettingsPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "settings", settingsPage{
		pageBase: s.base("设置", "settings"),
		Settings: s.api.Settings(),
	})
}

func (s *Server) handleOverviewJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, overviewResponseFrom(s.api.Overview()))
}

func (s *Server) handleClientsJSON(w http.ResponseWriter, r *http.Request) {
	clients := s.api.Clients()
	if clients == nil {
		clients = []view.ClientView{}
	}
	writeJSON(w, http.StatusOK, clients)
}

func (s *Server) handleClientJSON(w http.ResponseWriter, r *http.Request) {
	client, ok := s.api.Client(r.PathValue("id"))
	if !ok {
		writeJSONErrorCode(w, http.StatusNotFound, "not_found", "客户端不存在")
		return
	}
	writeJSON(w, http.StatusOK, client)
}

func (s *Server) handleMappingsJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, mappingEntries(s.api.Clients()))
}

func (s *Server) handleConfigSummaryJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.configSummary())
}

func (s *Server) handleSettingsJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.api.Settings())
}

// passwordRequest accepts either "password" (the reveal API shape) or
// "current_password" (the form-oriented mutations). Both are the current web
// administrator password and are never logged.
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONErrorCode(w http.ResponseWriter, status int, code, message string) {
	payload := map[string]string{"error": message}
	if code != "" {
		payload["code"] = code
	}
	writeJSON(w, status, payload)
}
