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
	"time"

	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/view"
	"hy2tun/internal/webui/shared"
)

// API is the subset of the server used by the web console.
type API interface {
	Overview() view.Overview
	Clients() []view.ClientView
	Client(id string) (view.ClientView, bool)
	PushMappings(id string, baseRevision int64, mappings []config.Mapping) (int64, error)
}

// Server is an authenticated HTTP server for the hy2tund web console.
type Server struct {
	api API
	cfg config.WebConfig
	log *slog.Logger

	sessions *shared.SessionManager
	limiter  *shared.LoginLimiter
	login    *shared.LoginHandler

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
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("serverui: static assets: %w", err)
	}

	s := &Server{
		api:           api,
		cfg:           cfg,
		log:           log,
		sessions:      sessions,
		limiter:       shared.NewLoginLimiter(5, time.Minute, time.Minute),
		staticFS:      staticFS,
		eventInterval: 2 * time.Second,
		done:          make(chan struct{}),
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
				s.render(w, status, "login", loginPage{Username: s.cfg.Username, Error: message})
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
	s.render(w, http.StatusOK, "login", loginPage{Username: s.cfg.Username, Error: message})
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
		Summary:  s.configSummary(),
		Overview: s.api.Overview(),
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
