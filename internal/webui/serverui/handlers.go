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
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/credentials"
	"hy2tun/internal/view"
	"hy2tun/internal/webui/shared"
)

// API is the subset of the server used by the web console.
type API interface {
	Overview() view.Overview
	Clients() []view.ClientView
	Client(id string) (view.ClientView, bool)
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
	mux.Handle("PUT /api/clients/{id}/mappings", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handlePushMappings))))
	mux.Handle("GET /api/mappings", s.auth(http.HandlerFunc(s.handleMappingsJSON)))
	mux.Handle("GET /api/events", s.auth(http.HandlerFunc(s.handleEvents)))
	mux.Handle("GET /api/config/summary", s.auth(http.HandlerFunc(s.handleConfigSummaryJSON)))

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
type passwordRequest struct {
	Password        string `json:"password"`
	CurrentPassword string `json:"current_password"`
}

func (p passwordRequest) secret() string {
	if p.CurrentPassword != "" {
		return p.CurrentPassword
	}
	return p.Password
}

func decodeSettingsBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_body", "请求体不是有效的 JSON")
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_body", "请求体包含多余内容")
		return false
	}
	return true
}

// confirmCurrentPassword implements the re-authentication required by every
// sensitive settings mutation. Failed attempts are rate-limited per client IP
// so a stolen session cannot brute-force the web password. The submitted value
// is never logged.
func (s *Server) confirmCurrentPassword(w http.ResponseWriter, r *http.Request, password string) bool {
	ip := shared.ClientIPWithProxy(r, s.trustProxy.Load())
	if !s.settingsLimiter.Allow(ip) {
		w.Header().Set("Retry-After", "60")
		writeJSONErrorCode(w, http.StatusTooManyRequests, "rate_limited", "尝试次数过多，请稍后再试")
		return false
	}
	if s.sessions.VerifyPassword(password) {
		s.settingsLimiter.Success(ip)
		return true
	}
	s.settingsLimiter.Failure(ip)
	writeJSONErrorCode(w, http.StatusForbidden, "invalid_password", "当前密码不正确")
	return false
}

func (s *Server) writeSettingsMutationError(w http.ResponseWriter, err error, code, message string) {
	status := http.StatusBadRequest
	if strings.Contains(err.Error(), "save server config") || strings.Contains(err.Error(), "save config") {
		status = http.StatusInternalServerError
		code = "internal"
		message = "写入配置文件失败"
	}
	if message == "" {
		message = err.Error()
	}
	s.log.Warn("settings mutation failed", "code", code, "error", err)
	writeJSONErrorCode(w, status, code, message)
}

func (s *Server) handleUpdateWebSettings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		passwordRequest
		Username        string `json:"username"`
		NewPassword     string `json:"new_password"`
		ConfirmPassword string `json:"confirm_password"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	// Serialize the complete verify -> hash -> persist -> session-update
	// sequence. The server's UpdateWebConfig also serializes its persist and
	// SetAdmin step; this lock prevents two handler requests from verifying
	// the same old password and racing each other's session updates.
	s.webAdminMu.Lock()
	defer s.webAdminMu.Unlock()
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	username := strings.TrimSpace(request.Username)
	if username == "" {
		writeJSONErrorCode(w, http.StatusBadRequest, "username_required", "用户名不能为空")
		return
	}
	if len([]rune(request.NewPassword)) < 8 {
		writeJSONErrorCode(w, http.StatusBadRequest, "weak_password", "新密码至少需要 8 个字符")
		return
	}
	if request.ConfirmPassword != "" && request.ConfirmPassword != request.NewPassword {
		writeJSONErrorCode(w, http.StatusBadRequest, "password_mismatch", "两次输入的新密码不一致")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(request.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		s.log.Error("hash web password", "error", err)
		writeJSONErrorCode(w, http.StatusInternalServerError, "internal", "无法保存新密码")
		return
	}
	if err := s.api.UpdateWebConfig(username, string(hash)); err != nil {
		s.writeSettingsMutationError(w, err, "invalid_web_config", "更新 Web 管理员失败")
		return
	}
	// UpdateWebConfig persists and updates the live login credentials under
	// the server's settings lock. The handler must not update sessions itself:
	// doing so could overwrite a newer concurrent update.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpdatePorts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		passwordRequest
		Allow                 []string `json:"allow"`
		Deny                  []string `json:"deny"`
		AutoRange             string   `json:"auto_range"`
		OfflineReserveSeconds int64    `json:"offline_reserve_seconds"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	if request.OfflineReserveSeconds < 1 {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_ports", "离线预留时间至少需要 1 秒")
		return
	}
	if err := s.api.UpdatePorts(request.Allow, request.Deny, request.AutoRange, time.Duration(request.OfflineReserveSeconds)*time.Second); err != nil {
		s.writeSettingsMutationError(w, err, "invalid_ports", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.api.Settings())
}

func (s *Server) handleUpdateTrustProxy(w http.ResponseWriter, r *http.Request) {
	var request struct {
		passwordRequest
		TrustProxy bool `json:"trust_proxy"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	if err := s.api.SetTrustProxy(request.TrustProxy); err != nil {
		s.writeSettingsMutationError(w, err, "invalid_proxy_config", "更新代理信任失败")
		return
	}
	// Update the live login/settings-confirmation limiters only after the
	// configuration write succeeded.
	s.SetTrustProxy(request.TrustProxy)
	writeJSON(w, http.StatusOK, s.api.Settings())
}

type rotateCredentialsResponse struct {
	Generation int64                   `json:"generation"`
	Results    []view.CredentialResult `json:"results"`
}

func (s *Server) handleRotateCredentials(w http.ResponseWriter, r *http.Request) {
	var request struct {
		passwordRequest
		Auth         string `json:"auth"`
		ControlToken string `json:"control_token"`
		SyncMode     string `json:"sync_mode"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	syncMode := strings.TrimSpace(request.SyncMode)
	switch syncMode {
	case "":
		syncMode = "manual"
	case "auto", "manual":
	default:
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_sync_mode", "同步方式必须是 auto 或 manual")
		return
	}
	generation, results, err := s.api.RotateCredentials(request.Auth, request.ControlToken, syncMode)
	if err != nil {
		code := "invalid_credentials"
		switch err.Error() {
		case "unsupported_auth_type":
			code = "unsupported_auth_type"
		case "no_change":
			code = "no_change"
		}
		s.writeSettingsMutationError(w, err, code, err.Error())
		return
	}
	if results == nil {
		results = []view.CredentialResult{}
	}
	writeJSON(w, http.StatusOK, rotateCredentialsResponse{Generation: generation, Results: results})
}

func (s *Server) handleRevokeCredentials(w http.ResponseWriter, r *http.Request) {
	var request passwordRequest
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	if err := s.api.RevokePreviousCredentials(); err != nil {
		s.writeSettingsMutationError(w, err, "revoke_failed", "撤销旧凭据失败")
		return
	}
	s.log.Info("previous credentials revoked from web console")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

type revealedCredential struct {
	Generation   int64  `json:"generation"`
	Auth         string `json:"auth"`
	ControlToken string `json:"control_token"`
}

type revealCredentialsResponse struct {
	Current  *revealedCredential `json:"current"`
	Previous *revealedCredential `json:"previous,omitempty"`
}

func (s *Server) handleRevealCredentials(w http.ResponseWriter, r *http.Request) {
	var request passwordRequest
	if !decodeSettingsBody(w, r, &request) {
		return
	}

	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}

	set := s.api.CredentialsSnapshot()
	if set == nil {
		writeJSONErrorCode(w, http.StatusInternalServerError, "internal", "凭据不可用")
		return
	}
	payload := revealCredentialsResponse{
		Current: &revealedCredential{
			Generation:   set.Generation,
			Auth:         set.Auth,
			ControlToken: set.ControlToken,
		},
	}
	if set.PreviousGeneration > 0 {
		payload.Previous = &revealedCredential{
			Generation:   set.PreviousGeneration,
			Auth:         set.PreviousAuth,
			ControlToken: set.PreviousControlToken,
		}
	}
	// writeJSON sets this too; set it before the body is written so even a
	// later encoder error cannot downgrade the cache policy.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, payload)
}

type pushRequest struct {
	BaseRevision int64            `json:"base_revision"`
	Mappings     []config.Mapping `json:"mappings"`
}

type pushResponse struct {
	Revision int64 `json:"revision"`
}

type pushResult struct {
	ID           string `json:"id"`
	OK           bool   `json:"ok"`
	AssignedPort int    `json:"assigned_port"`
	Error        string `json:"error"`
}

type pushErrorResponse struct {
	Error   string       `json:"error"`
	Code    string       `json:"code"`
	Results []pushResult `json:"results,omitempty"`
}

func (s *Server) handlePushMappings(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	if clientID == "" {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_mapping", "客户端 ID 不能为空")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	var request pushRequest
	if err := decoder.Decode(&request); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_body", "请求体不是有效的 JSON")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_body", "请求体包含多余内容")
		return
	}
	if request.BaseRevision < 0 {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_revision", "base_revision 不能为负数")
		return
	}
	if err := validatePushMappings(request.Mappings); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_mapping", err.Error())
		return
	}

	revision, err := s.api.PushMappings(clientID, request.BaseRevision, request.Mappings)
	if err != nil {
		s.writePushError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pushResponse{Revision: revision})
}

func validatePushMappings(mappings []config.Mapping) error {
	seen := make(map[string]struct{}, len(mappings))
	for i, mapping := range mappings {
		field := fmt.Sprintf("mappings[%d]", i)
		if mapping.ID == "" {
			return fmt.Errorf("%s.id 不能为空", field)
		}
		if _, duplicate := seen[mapping.ID]; duplicate {
			return fmt.Errorf("%s.id %q 重复", field, mapping.ID)
		}
		seen[mapping.ID] = struct{}{}

		if mapping.Type != "tcp" && mapping.Type != "udp" {
			return fmt.Errorf("%s.type 必须是 tcp 或 udp", field)
		}
		if mapping.RemotePort < 0 || mapping.RemotePort > 65535 {
			return fmt.Errorf("%s.remote_port 必须在 0 到 65535 之间", field)
		}
		host, portText, err := net.SplitHostPort(mapping.LocalAddr)
		if err != nil {
			return fmt.Errorf("%s.local_addr 必须是 host:port", field)
		}
		if host == "" {
			return fmt.Errorf("%s.local_addr 必须包含主机", field)
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("%s.local_addr 端口无效", field)
		}
		if len([]rune(mapping.Note)) > 128 {
			return fmt.Errorf("%s.note 不能超过 128 个字符", field)
		}
	}
	return nil
}

func (s *Server) writePushError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, control.ErrConflict):
		writeJSONErrorCode(w, http.StatusConflict, "conflict", "客户端配置已变化，请刷新后重试")
		return
	case errors.Is(err, control.ErrClientOffline):
		writeJSONErrorCode(w, http.StatusConflict, "client_offline", "客户端离线，无法修改配置")
		return
	case errors.Is(err, control.ErrClientNotReady):
		writeJSONErrorCode(w, http.StatusConflict, "client_not_ready", "客户端正在连接，请稍后重试")
		return
	case errors.Is(err, control.ErrPushTimeout):
		writeJSONErrorCode(w, http.StatusGatewayTimeout, "push_timeout", "客户端未在超时时间内确认配置")
		return
	}

	var provider interface{ MappingResults() []control.Result }
	if errors.As(err, &provider) {
		results := provider.MappingResults()
		status := http.StatusConflict
		code := "push_rejected"
		for _, result := range results {
			if result.Error == "" {
				continue
			}
			if code == "push_rejected" {
				code = result.Error
			}
			switch result.Error {
			case string(control.CodeBadMessage), string(control.CodeInvalidTarget):
				status = http.StatusBadRequest
			}
		}

		payload := pushErrorResponse{Error: "映射配置被拒绝", Code: code}
		payload.Results = make([]pushResult, 0, len(results))
		for _, result := range results {
			payload.Results = append(payload.Results, pushResult{
				ID:           result.ID,
				OK:           result.OK,
				AssignedPort: result.AssignedPort,
				Error:        result.Error,
			})
		}
		writeJSON(w, status, payload)
		return
	}

	s.log.Warn("mapping push failed", "error", err)
	writeJSONErrorCode(w, http.StatusInternalServerError, "internal", "推送配置失败")
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONErrorCode(w, http.StatusInternalServerError, "internal", "当前连接不支持流式响应")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "retry: 3000\n\n")
	flusher.Flush()

	send := func() bool {
		data, err := json.Marshal(s.snapshot())
		if err != nil {
			s.log.Error("encode web event", "error", err)
			return false
		}
		if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !send() {
		return
	}

	interval := s.eventInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-ticker.C:
			if !send() {
				return
			}
		}
	}
}

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
