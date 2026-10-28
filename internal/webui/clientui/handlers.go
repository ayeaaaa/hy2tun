// Package clientui implements the hy2tun client web console: an authenticated,
// server-rendered view of the local connection, mappings, logs, and read-only
// settings.
package clientui

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
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
	"time"

	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/config"
	"hy2tun/internal/credentials"
	"hy2tun/internal/view"
	"hy2tun/internal/webui/shared"
)

// API is the subset of the client used by the web console. Configuration edits
// always go through these methods so the client keeps its atomic-write and
// revision semantics.
type API interface {
	Status() view.ClientStatus
	Mappings() []view.MappingView
	AddMapping(config.Mapping) error
	UpdateMapping(config.Mapping) error
	DeleteMapping(id string) error
	Logs(level string, limit int) []view.LogEntry

	Settings() SettingsView
	UpdateWebConfig(username, passwordHash string) error
	UpdateName(name string) error
	RotateLocalCredentials(generation int64, auth, controlToken string) error
	CredentialsSnapshot() *credentials.Set
}

// ClientAPI is an alias for API.
type ClientAPI = API

// Server is an authenticated HTTP server for the hy2tun client console.
type Server struct {
	api API
	cfg config.WebConfig
	log *slog.Logger

	sessions *shared.SessionManager
	limiter  *shared.LoginLimiter
	login    *shared.LoginHandler
	// settingsLimiter rate-limits failed current-password confirmations for
	// sensitive settings mutations so a stolen session cannot brute-force the
	// web password. webAdminMu serializes the verify -> hash -> persist ->
	// session-update sequence.
	settingsLimiter *shared.LoginLimiter
	webAdminMu      sync.Mutex

	pages    map[string]*template.Template
	handler  http.Handler
	staticFS fs.FS

	eventInterval time.Duration
	done          chan struct{}

	mu         sync.Mutex
	started    bool
	closed     bool
	listener   net.Listener
	httpServer *http.Server

	closeOnce sync.Once
	closeErr  error
}

// New validates cfg, parses the embedded templates, and returns a client web
// console that has not started listening yet.
func New(api API, cfg config.WebConfig, sessionKey []byte, log *slog.Logger) (*Server, error) {
	if api == nil {
		return nil, fmt.Errorf("clientui: API is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.Username == "" {
		cfg.Username = "admin"
	}
	if cfg.PasswordHash == "" {
		return nil, fmt.Errorf("clientui: web password hash is required")
	}
	if _, err := bcrypt.Cost([]byte(cfg.PasswordHash)); err != nil {
		return nil, fmt.Errorf("clientui: web password hash is invalid: %w", err)
	}
	if (cfg.TLS.Cert == "") != (cfg.TLS.Key == "") {
		return nil, fmt.Errorf("clientui: web TLS cert and key must be set together")
	}

	sessions, err := shared.NewSessionManager(sessionKey, 7*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("clientui: %w", err)
	}
	// Seed the live administrator so settings re-authentication works before
	// the first web-admin update. UpdateAdmin also revokes any sessions, but
	// none exist yet.
	sessions.UpdateAdmin(cfg.Username, cfg.PasswordHash)
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("clientui: static assets: %w", err)
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
				s.render(w, status, "login", loginPage{Username: s.adminUsername(), Error: message})
				return
			}
			writeJSONErrorCode(w, status, "", message)
		},
	}

	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	s.handler = s.newHandler()
	return s, nil
}

// NewClient is an alias for New.
func NewClient(api API, cfg config.WebConfig, sessionKey []byte, log *slog.Logger) (*Server, error) {
	return New(api, cfg, sessionKey, log)
}

// Handler returns the HTTP handler. It is usable without Start for tests or for
// embedding behind another listener.
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

// Start binds the web listener and serves in the background. HTTPS is used when
// web.tls.cert/key are configured.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("clientui: web server is closed")
	}
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("clientui: web server already started")
	}
	if strings.TrimSpace(s.cfg.Listen) == "" {
		s.mu.Unlock()
		return fmt.Errorf("clientui: web.listen is empty")
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
			return fail(fmt.Errorf("clientui: load web TLS certificate: %w", err))
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	listener, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fail(fmt.Errorf("clientui: listen web %s: %w", s.cfg.Listen, err))
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
		return fail(fmt.Errorf("clientui: web server is closed"))
	}
	s.listener = listener
	s.httpServer = httpServer
	s.mu.Unlock()

	scheme := "http"
	if tlsConfig != nil {
		scheme = "https"
	}
	s.log.Info("client web console listening", "address", listener.Addr().String(), "scheme", scheme)

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
			s.log.Error("client web console stopped", "error", serveErr)
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

	mux.Handle("GET /{$}", s.auth(http.HandlerFunc(s.handlePage)))
	mux.Handle("GET /api/status", s.auth(http.HandlerFunc(s.handleStatusJSON)))
	mux.Handle("GET /api/mappings", s.auth(http.HandlerFunc(s.handleMappingsJSON)))
	mux.Handle("POST /api/mappings", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleAddMapping))))
	mux.Handle("PUT /api/mappings/{id}", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleUpdateMapping))))
	mux.Handle("DELETE /api/mappings/{id}", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleDeleteMapping))))
	mux.Handle("GET /api/logs", s.auth(http.HandlerFunc(s.handleLogsJSON)))
	mux.Handle("GET /api/events", s.auth(http.HandlerFunc(s.handleEvents)))

	mux.Handle("GET /api/settings", s.auth(http.HandlerFunc(s.handleSettingsJSON)))
	mux.Handle("PUT /api/settings/web", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleUpdateWebSettings))))
	mux.Handle("PUT /api/settings/name", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleUpdateName))))
	mux.Handle("PUT /api/settings/credentials", s.auth(shared.RequireCSRF(http.HandlerFunc(s.handleRotateCredentials))))
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
	s.render(w, http.StatusOK, "login", loginPage{Username: s.adminUsername(), Error: message})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Clear(w, r)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	status := s.api.Status()
	mappings := s.api.Mappings()
	if mappings == nil {
		mappings = []view.MappingView{}
	}
	logs := s.api.Logs("", 200)
	if logs == nil {
		logs = []view.LogEntry{}
	}
	s.render(w, http.StatusOK, "index", indexPage{
		pageBase:       s.base("端口映射"),
		Status:         status,
		Mappings:       mappings,
		Logs:           logs,
		Settings:       s.settingsFor(),
		ActiveMappings: countActiveMappings(mappings),
	})
}

func (s *Server) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	mappings := s.api.Mappings()
	writeJSON(w, http.StatusOK, statusResponseFrom(s.api.Status(), mappings))
}

func (s *Server) handleMappingsJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, mappingResponses(s.api.Mappings()))
}

func (s *Server) handleAddMapping(w http.ResponseWriter, r *http.Request) {
	mapping, err := decodeMapping(w, r)
	if err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if mapping.ID == "" {
		mapping.ID = newMappingID()
	}
	if err := validateMapping(mapping); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_mapping", err.Error())
		return
	}
	if err := s.api.AddMapping(mapping); err != nil {
		s.writeMappingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, mappingResponseFrom(view.MappingView{
		Mapping: mapping,
		Status:  "pending",
	}))
}

func (s *Server) handleUpdateMapping(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_mapping", "映射 ID 不能为空")
		return
	}
	mapping, err := decodeMapping(w, r)
	if err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_body", err.Error())
		return
	}
	if mapping.ID != "" && mapping.ID != id {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_mapping", "路径中的映射 ID 与请求体不一致")
		return
	}
	mapping.ID = id
	if err := validateMapping(mapping); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_mapping", err.Error())
		return
	}
	if err := s.api.UpdateMapping(mapping); err != nil {
		s.writeMappingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, mappingResponseFrom(view.MappingView{
		Mapping: mapping,
		Status:  "pending",
	}))
}

func (s *Server) handleDeleteMapping(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_mapping", "映射 ID 不能为空")
		return
	}
	if err := s.api.DeleteMapping(id); err != nil {
		s.writeMappingError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogsJSON(w http.ResponseWriter, r *http.Request) {
	level := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("level")))
	switch level {
	case "", "all", "debug", "info", "warn", "error":
	default:
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_level", "日志级别必须是 debug、info、warn 或 error")
		return
	}

	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 500 {
			writeJSONErrorCode(w, http.StatusBadRequest, "invalid_limit", "limit 必须在 1 到 500 之间")
			return
		}
		limit = parsed
	}

	entries := s.api.Logs(level, limit)
	if entries == nil {
		entries = []view.LogEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) handleSettingsJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsFor())
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
// sensitive settings mutation. Failed attempts are rate-limited per client IP.
// The submitted value is never logged.
func (s *Server) confirmCurrentPassword(w http.ResponseWriter, r *http.Request, password string) bool {
	ip := shared.ClientIPWithProxy(r, s.cfg.TrustProxy)
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
	if strings.Contains(err.Error(), "save client config") || strings.Contains(err.Error(), "write config") {
		status = http.StatusInternalServerError
		code = "internal"
		message = "写入配置文件失败"
	}
	if message == "" {
		message = err.Error()
	}
	s.log.Warn("client settings mutation failed", "code", code, "error", err)
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
	// sequence so two requests cannot both verify the old password and race
	// their session updates.
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
		s.log.Error("hash client web password", "error", err)
		writeJSONErrorCode(w, http.StatusInternalServerError, "internal", "无法保存新密码")
		return
	}
	if err := s.api.UpdateWebConfig(username, string(hash)); err != nil {
		s.writeSettingsMutationError(w, err, "invalid_web_config", "更新 Web 管理员失败")
		return
	}
	// UpdateWebConfig persists the config only. The live login path must be
	// updated here, and every existing session is revoked because the
	// administrator identity changed.
	s.sessions.UpdateAdmin(username, string(hash))
	s.sessions.RevokeAll()
	s.login.SetAdmin(username, string(hash))
	s.mu.Lock()
	s.cfg.Username = username
	s.cfg.PasswordHash = string(hash)
	s.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpdateName(w http.ResponseWriter, r *http.Request) {
	var request struct {
		passwordRequest
		Name string `json:"name"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		writeJSONErrorCode(w, http.StatusBadRequest, "name_required", "客户端名称不能为空")
		return
	}
	if err := s.api.UpdateName(name); err != nil {
		s.writeSettingsMutationError(w, err, "invalid_name", "更新客户端名称失败")
		return
	}
	writeJSON(w, http.StatusOK, s.settingsFor())
}

func (s *Server) handleRotateCredentials(w http.ResponseWriter, r *http.Request) {
	var request struct {
		passwordRequest
		Generation   int64  `json:"generation"`
		Auth         string `json:"auth"`
		ControlToken string `json:"control_token"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	if request.Generation < 1 {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_generation", "代际必须是正整数")
		return
	}
	if request.Auth == "" {
		writeJSONErrorCode(w, http.StatusBadRequest, "auth_required", "HY2 密码不能为空")
		return
	}
	if len(request.Auth) < 8 {
		writeJSONErrorCode(w, http.StatusBadRequest, "weak_auth", "HY2 密码至少需要 8 个字节")
		return
	}
	if request.ControlToken == "" {
		writeJSONErrorCode(w, http.StatusBadRequest, "control_token_required", "control token 不能为空")
		return
	}
	current := s.api.Settings().Credentials.Generation
	if current < 1 {
		current = 1
	}
	if request.Generation <= current {
		writeJSONErrorCode(w, http.StatusBadRequest, "generation_must_advance",
			fmt.Sprintf("新代际必须大于当前代际 %d", current))
		return
	}
	if err := s.api.RotateLocalCredentials(request.Generation, request.Auth, request.ControlToken); err != nil {
		s.writeSettingsMutationError(w, err, "invalid_credentials", err.Error())
		return
	}
	// The client writes the config and forces a reconnect before returning.
	// The new generation is only confirmed after the next hello_ack, so the
	// UI polls /api/status rather than waiting here.
	writeJSON(w, http.StatusAccepted, map[string]any{
		"generation": request.Generation,
		"status":     "pending",
	})
}

type revealedCredential struct {
	Generation   int64  `json:"generation"`
	Auth         string `json:"auth"`
	ControlToken string `json:"control_token"`
}

type revealCredentialsResponse struct {
	Current *revealedCredential `json:"current"`
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
	// writeJSON sets this too; set it before the body is written so even a
	// later encoder error cannot downgrade the cache policy.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, payload)
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
			s.log.Error("encode client web event", "error", err)
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

func (s *Server) snapshot() sseSnapshot {
	mappings := s.api.Mappings()
	if mappings == nil {
		mappings = []view.MappingView{}
	}
	return sseSnapshot{
		Status:     statusResponseFrom(s.api.Status(), mappings),
		Mappings:   mappingResponses(mappings),
		ServerTime: time.Now(),
	}
}

func decodeMapping(w http.ResponseWriter, r *http.Request) (config.Mapping, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	var mapping config.Mapping
	if err := decoder.Decode(&mapping); err != nil {
		return config.Mapping{}, fmt.Errorf("请求体不是有效的 JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return config.Mapping{}, fmt.Errorf("请求体包含多余内容")
	}
	return mapping, nil
}

func validateMapping(mapping config.Mapping) error {
	switch mapping.Type {
	case "tcp", "udp":
	default:
		return fmt.Errorf("type 必须是 tcp 或 udp")
	}
	if mapping.RemotePort < 0 || mapping.RemotePort > 65535 {
		return fmt.Errorf("remote_port 必须在 0 到 65535 之间")
	}
	host, portText, err := net.SplitHostPort(mapping.LocalAddr)
	if err != nil {
		return fmt.Errorf("local_addr 必须是 host:port")
	}
	if host == "" {
		return fmt.Errorf("local_addr 必须包含主机")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("local_addr 端口无效")
	}
	if len([]rune(mapping.Note)) > 128 {
		return fmt.Errorf("note 不能超过 128 个字符")
	}
	return nil
}

func (s *Server) writeMappingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, config.ErrMappingNotFound):
		writeJSONErrorCode(w, http.StatusNotFound, "not_found", "映射不存在")
	case errors.Is(err, config.ErrMappingExists):
		writeJSONErrorCode(w, http.StatusConflict, "conflict", "映射 ID 已存在")
	default:
		s.log.Warn("client mapping edit failed", "error", err)
		writeJSONErrorCode(w, http.StatusInternalServerError, "internal", "保存映射失败")
	}
}

func newMappingID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("m-%08x", time.Now().UnixNano()&0xffffffff)
	}
	return "m-" + hex.EncodeToString(b[:])
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
