// Package server implements the hy2tund HY2 server. It serves standard
// Hysteria 2 TCP and UDP proxy traffic and, in later tasks, also hosts the
// hy2tun control stream and reverse data plane on reserved virtual addresses.
package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hyserver "github.com/apernet/hysteria/core/v2/server"
	"github.com/apernet/hysteria/extras/v2/auth"
	"github.com/apernet/hysteria/extras/v2/obfs"
	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/config"
	"hy2tun/internal/credentials"
	"hy2tun/internal/ports"
	"hy2tun/internal/state"
	"hy2tun/internal/version"
	"hy2tun/internal/view"
	"hy2tun/internal/webui/serverui"
)

// Server is a running hy2tund HY2 server.
type Server struct {
	// settingsMu guards cfg. Hot settings updates build a copy, persist it, and
	// only then replace the pointer, so readers always observe a complete
	// configuration. Use currentConfig instead of reading cfg directly.
	settingsMu sync.RWMutex
	cfg        *config.ServerConfig
	log        *slog.Logger

	hyServer   hyserver.Server
	packetConn net.PacketConn
	listenAddr net.Addr

	store    *state.Store
	mappings *mappingManager
	control  *controlHandler
	data     *dataStreams
	web      *serverui.Server

	// credentials is the live credential pair used by the HY2 authenticator
	// and the control-token check. It is replaced atomically on rotation.
	credentials *credentials.Store

	// rotationMu serializes credential rotation and revoke. Auto-push holds it
	// while waiting for client acks, so two rotations cannot interleave.
	rotationMu sync.Mutex

	// trustProxy mirrors cfg.Web.TrustProxy for readers that must not hold
	// settingsMu, and is updated together with the web console's login handler.
	trustProxy atomic.Bool

	// saveConfig persists a configuration file. It is a field so tests can
	// inject write failures; production uses config.SaveServer.
	saveConfig func(path string, cfg *config.ServerConfig) error

	startedAt time.Time
	started   atomic.Bool
	closed    atomic.Bool

	closeOnce sync.Once
	closeErr  error
}

// New validates cfg, loads the TLS key pair, binds the UDP socket, and creates
// the Hysteria server. It does not start serving; call Start for that.
func New(cfg *config.ServerConfig, log *slog.Logger) (*Server, error) {
	if cfg == nil {
		return nil, fmt.Errorf("server config is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	if cfg.TLS.Cert == "" || cfg.TLS.Key == "" {
		return nil, fmt.Errorf("tls.cert and tls.key are required")
	}
	cert, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair: %w", err)
	}
	credStore := credentials.NewStore(credentialSetFromConfig(cfg))
	authenticator, err := newAuthenticator(&cfg.Auth, credStore)
	if err != nil {
		return nil, err
	}

	packetConn, err := net.ListenPacket("udp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen UDP %s: %w", cfg.Listen, err)
	}

	s := &Server{
		cfg:         cfg,
		log:         log,
		packetConn:  packetConn,
		listenAddr:  packetConn.LocalAddr(),
		startedAt:   time.Now(),
		data:        newDataStreams(),
		credentials: credStore,
		saveConfig:  config.SaveServer,
	}
	s.trustProxy.Store(cfg.Web.TrustProxy)

	policy, err := ports.NewPolicy(cfg.Ports.Allow, cfg.Ports.Deny, cfg.Ports.AutoRange)
	if err != nil {
		_ = packetConn.Close()
		return nil, fmt.Errorf("create port policy: %w", err)
	}
	if err := s.initState(); err != nil {
		_ = packetConn.Close()
		return nil, err
	}
	s.mappings = newMappingManager(policy, s.store, log, cfg.ListenHost, s.reservedPorts(), s.data)
	s.mappings.serveTCP = s.serveTCP
	s.control = newControlHandler(s, log)

	conn := packetConn
	if cfg.Obfs.Type != "" {
		if cfg.Obfs.Type != "salamander" {
			_ = packetConn.Close()
			return nil, fmt.Errorf("unsupported obfs.type %q", cfg.Obfs.Type)
		}
		conn, err = obfs.WrapPacketConnSalamander(packetConn, []byte(cfg.Obfs.Salamander.Password))
		if err != nil {
			_ = packetConn.Close()
			return nil, fmt.Errorf("configure salamander obfs: %w", err)
		}
	}

	hyConfig := &hyserver.Config{
		TLSConfig: hyserver.TLSConfig{
			Certificates: []tls.Certificate{cert},
		},
		QUICConfig: hyserver.QUICConfig{
			InitialStreamReceiveWindow:     cfg.QUIC.InitialStreamReceiveWindow,
			MaxStreamReceiveWindow:         cfg.QUIC.MaxStreamReceiveWindow,
			InitialConnectionReceiveWindow: cfg.QUIC.InitialConnectionReceiveWindow,
			MaxConnectionReceiveWindow:     cfg.QUIC.MaxConnectionReceiveWindow,
			MaxIdleTimeout:                 time.Duration(cfg.QUIC.MaxIdleTimeout),
			MaxIncomingStreams:             cfg.QUIC.MaxIncomingStreams,
		},
		BandwidthConfig: hyserver.BandwidthConfig{
			MaxTx: uint64(cfg.Bandwidth.Up),
			MaxRx: uint64(cfg.Bandwidth.Down),
		},
		IgnoreClientBandwidth: cfg.Bandwidth.IgnoreClient,
		Conn:                  conn,
		Outbound:              &outbound{server: s},
		DisableUDP:            false,
		Authenticator:         authenticator,
		EventLogger:           newEventLogger(log),
	}
	hyServer, err := hyserver.NewServer(hyConfig)
	if err != nil {
		_ = packetConn.Close()
		return nil, fmt.Errorf("create HY2 server: %w", err)
	}
	s.hyServer = hyServer
	if cfg.Web.Listen != "" {
		sessionKey, err := loadWebSessionKey(cfg, log)
		if err != nil {
			_ = hyServer.Close()
			_ = packetConn.Close()
			if s.store != nil {
				_ = s.store.Close()
			}
			return nil, err
		}
		web, err := serverui.New(s, cfg.Web, sessionKey, log)
		if err != nil {
			_ = hyServer.Close()
			_ = packetConn.Close()
			if s.store != nil {
				_ = s.store.Close()
			}
			return nil, fmt.Errorf("create web console: %w", err)
		}
		s.web = web
	}
	go s.data.janitor()

	log.Debug("HY2 server initialized", "listen", s.listenAddr.String())
	return s, nil
}

// newAuthenticator maps the config auth mode to a Hysteria authenticator.
// Password mode reads the live credential store so rotations take effect
// without a restart; userpass mode keeps the static user table.
func newAuthenticator(authCfg *config.AuthConfig, credStore *credentials.Store) (hyserver.Authenticator, error) {
	switch authCfg.Type {
	case "password":
		return newDynamicAuthenticator(credStore, nil), nil
	case "userpass":
		return auth.NewUserPassAuthenticator(authCfg.Users), nil
	default:
		return nil, fmt.Errorf("unsupported auth.type %q", authCfg.Type)
	}
}

// Start serves HY2 traffic and blocks until the server fails or ctx is done.
// Cancelling ctx closes the server and returns nil. Callers should call Close
// if Start returns a serve error.
func (s *Server) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.closed.Load() {
		return fmt.Errorf("server is closed")
	}
	if !s.started.CompareAndSwap(false, true) {
		return fmt.Errorf("server already started")
	}
	if s.hyServer == nil {
		return fmt.Errorf("server is not initialized")
	}

	if s.web != nil {
		if err := s.web.Start(); err != nil {
			return fmt.Errorf("start web console: %w", err)
		}
	}

	s.log.Info("HY2 server listening", "listen", s.listenAddr.String())

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.hyServer.Serve()
	}()

	select {
	case err := <-serveErr:
		if s.closed.Load() {
			return nil
		}
		return fmt.Errorf("serve HY2: %w", err)
	case <-ctx.Done():
		return s.Close()
	}
}

// Close stops the HY2 server and closes the UDP socket. It is idempotent and
// returns the same error on every call.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		var errs []error
		if s.web != nil {
			if err := s.web.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close web console: %w", err))
			}
		}
		if s.hyServer != nil {
			if err := s.hyServer.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close HY2 server: %w", err))
			}
		}
		// The Hysteria server owns the packet connection and closes it too.
		// Close it explicitly as well, ignoring the expected double close.
		if s.packetConn != nil {
			if err := s.packetConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, fmt.Errorf("close UDP socket: %w", err))
			}
		}
		if s.mappings != nil {
			s.mappings.closeAll()
		}
		if s.data != nil {
			s.data.closeAll()
		}
		if s.store != nil {
			if err := s.store.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close state: %w", err))
			}
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

// ListenAddr returns the bound UDP listen address.
func (s *Server) ListenAddr() net.Addr {
	return s.listenAddr
}

// WebAddr returns the bound web listener address, or the configured address
// when the web console is disabled or has not started yet.
func (s *Server) WebAddr() string {
	if s.web != nil {
		return s.web.Addr()
	}
	return s.currentConfig().Web.Listen
}

// currentConfig returns the configuration currently in effect. Hot updates
// replace the pointer under settingsMu; callers must treat the result as
// read-only.
func (s *Server) currentConfig() *config.ServerConfig {
	s.settingsMu.RLock()
	defer s.settingsMu.RUnlock()
	return s.cfg
}

// cloneConfig returns a deep-enough copy for a hot update. Only fields that
// hot updates replace are copied; nested maps that are never mutated may be
// shared.
func cloneConfig(cfg *config.ServerConfig) *config.ServerConfig {
	if cfg == nil {
		return nil
	}
	next := *cfg
	next.Ports.Allow = append([]string(nil), cfg.Ports.Allow...)
	next.Ports.Deny = append([]string(nil), cfg.Ports.Deny...)
	return &next
}

// persistConfig writes next with the configured save function. Configurations
// created programmatically without a path have nothing to persist; the hot
// update still applies at runtime.
func (s *Server) persistConfig(next *config.ServerConfig) error {
	if next == nil || next.ConfigPath == "" {
		return nil
	}
	save := s.saveConfig
	if save == nil {
		save = config.SaveServer
	}
	if err := save(next.ConfigPath, next); err != nil {
		return err
	}
	return nil
}

// UpdatePorts validates and hot-applies a port policy and offline reservation
// duration. The configuration file is written first; if that fails, the
// running policy and store are left unchanged. Mappings that are already bound
// stay running even when the new allow list no longer covers them.
func (s *Server) UpdatePorts(allow, deny []string, autoRange string, offlineReserve time.Duration) error {
	if s == nil {
		return fmt.Errorf("update ports: server is nil")
	}
	if offlineReserve < time.Second {
		return fmt.Errorf("update ports: offline reserve must be at least 1 second")
	}

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	next := cloneConfig(s.cfg)
	if next == nil {
		return fmt.Errorf("update ports: server config is nil")
	}
	next.Ports.Allow = append([]string(nil), allow...)
	next.Ports.Deny = append([]string(nil), deny...)
	next.Ports.AutoRange = autoRange
	next.Ports.OfflineReserve = config.Duration(offlineReserve)
	// Validate applies the documented defaults (for example an empty
	// auto_range) and checks every port specification with the same parser
	// used at load time.
	if err := next.Validate(); err != nil {
		return fmt.Errorf("update ports: %w", err)
	}
	policy, err := ports.NewPolicy(next.Ports.Allow, next.Ports.Deny, next.Ports.AutoRange)
	if err != nil {
		return fmt.Errorf("update ports: %w", err)
	}
	if err := s.persistConfig(next); err != nil {
		return fmt.Errorf("update ports: %w", err)
	}

	s.cfg = next
	if s.mappings != nil {
		s.mappings.SetPolicy(policy)
	}
	if s.store != nil {
		s.store.SetOfflineReserve(time.Duration(next.Ports.OfflineReserve))
	}
	s.log.Info("port policy updated",
		"allow", next.Ports.Allow,
		"deny", next.Ports.Deny,
		"auto_range", next.Ports.AutoRange,
		"offline_reserve", time.Duration(next.Ports.OfflineReserve).String(),
	)
	return nil
}

// SetTrustProxy hot-applies whether forwarded headers are trusted when
// deriving client IPs. The configuration file is written first; on failure the
// runtime setting is unchanged.
func (s *Server) SetTrustProxy(trust bool) error {
	if s == nil {
		return fmt.Errorf("set trust proxy: server is nil")
	}

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	next := cloneConfig(s.cfg)
	if next == nil {
		return fmt.Errorf("set trust proxy: server config is nil")
	}
	next.Web.TrustProxy = trust
	if err := s.persistConfig(next); err != nil {
		return fmt.Errorf("set trust proxy: %w", err)
	}

	s.cfg = next
	s.trustProxy.Store(trust)
	if s.web != nil {
		s.web.SetTrustProxy(trust)
	}
	s.log.Info("trust proxy updated", "trust_proxy", trust)
	return nil
}

// UpdateWebConfig hot-applies a new web administrator username and bcrypt
// password hash. The configuration file is written first; on failure neither
// the running configuration nor the live login credentials change. A
// successful update revokes every existing web session.
func (s *Server) UpdateWebConfig(username, passwordHash string) error {
	if s == nil {
		return fmt.Errorf("update web config: server is nil")
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("update web config: username is required")
	}
	if _, err := bcrypt.Cost([]byte(passwordHash)); err != nil {
		return fmt.Errorf("update web config: password hash is invalid: %w", err)
	}

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()

	next := cloneConfig(s.cfg)
	if next == nil {
		return fmt.Errorf("update web config: server config is nil")
	}
	next.Web.Username = username
	next.Web.PasswordHash = passwordHash
	if err := next.Validate(); err != nil {
		return fmt.Errorf("update web config: %w", err)
	}
	if err := s.persistConfig(next); err != nil {
		return fmt.Errorf("update web config: %w", err)
	}
	s.cfg = next
	// Update the live console credentials while still holding settingsMu.
	// Concurrent updates therefore serialize on the same lock: the last
	// successful write is the one reflected by both the config file and the
	// session manager.
	if s.web != nil {
		s.web.SetAdmin(username, passwordHash)
	}
	s.log.Info("web administrator updated", "username", username)
	return nil
}

// Settings returns the secret-free settings snapshot used by the web console.
// It merges the online-only credential statuses with offline clients from the
// state store so the sync table never hides a client that still needs the new
// credentials.
func (s *Server) Settings() serverui.SettingsView {
	cfg := s.currentConfig()
	if cfg == nil {
		return serverui.SettingsView{}
	}
	creds := s.currentCredentials()
	if creds == nil {
		creds = &credentials.Set{Generation: 1}
	}
	generation := creds.Generation
	if generation < 1 {
		generation = 1
	}

	webListen := cfg.Web.Listen
	if s.web != nil {
		webListen = s.web.Addr()
	}
	settings := serverui.SettingsView{
		WebUsername:           cfg.Web.Username,
		WebPasswordConfigured: cfg.Web.PasswordHash != "",
		AuthType:              cfg.Auth.Type,
		TrustProxy:            s.trustProxy.Load(),
		Ports: serverui.PortSettingsView{
			Allow:                 append([]string(nil), cfg.Ports.Allow...),
			Deny:                  append([]string(nil), cfg.Ports.Deny...),
			AutoRange:             cfg.Ports.AutoRange,
			OfflineReserveSeconds: int64(time.Duration(cfg.Ports.OfflineReserve) / time.Second),
			OfflineReserve:        time.Duration(cfg.Ports.OfflineReserve),
		},
		Credentials: serverui.CredentialSettingsView{
			Generation:                 generation,
			PreviousGeneration:         creds.PreviousGeneration,
			CurrentAuthMasked:          maskSecret(creds.Auth),
			CurrentControlTokenMasked:  maskSecret(creds.ControlToken),
			PreviousAuthMasked:         maskSecret(creds.PreviousAuth),
			PreviousControlTokenMasked: maskSecret(creds.PreviousControlToken),
			Rotatable:                  cfg.Auth.Type == "password",
		},
		ReadOnly: []serverui.ReadOnlySetting{
			{Label: "HY2 监听", Value: s.listenAddr.String()},
			{Label: "绑定地址", Value: valueOr(cfg.ListenHost, "全部地址")},
			{Label: "HY2 TLS", Value: boolLabel(cfg.TLS.Cert != "", "已启用", "未启用")},
			{Label: "Obfs", Value: valueOr(cfg.Obfs.Type, "未启用")},
			{Label: "QUIC 最大空闲", Value: time.Duration(cfg.QUIC.MaxIdleTimeout).String()},
			{Label: "Web 监听", Value: valueOr(webListen, "未启用")},
			{Label: "日志级别", Value: cfg.Log.Level},
			{Label: "状态文件", Value: valueOr(cfg.StateFile, "未配置")},
			{Label: "配置文件", Value: valueOr(cfg.ConfigPath, "—")},
		},
	}
	settings.Clients = s.credentialClients(generation)
	return settings
}

func (s *Server) credentialClients(currentGeneration int64) []serverui.ClientCredentialView {
	clients := s.Clients()
	byID := make(map[string]view.ClientView, len(clients))
	for _, client := range clients {
		byID[client.ID] = client
	}

	seen := make(map[string]struct{})
	out := make([]serverui.ClientCredentialView, 0, len(clients))
	for _, status := range s.CredentialStatuses() {
		item := serverui.ClientCredentialView{
			ClientID:   status.ClientID,
			Name:       status.Name,
			Online:     true,
			Generation: status.Generation,
			Status:     status.Status,
			Error:      status.Error,
		}
		if client, ok := byID[status.ClientID]; ok {
			item.Name = client.Name
			item.Online = client.Online
			item.LastSeen = client.LastSeen
		}
		seen[status.ClientID] = struct{}{}
		out = append(out, item)
	}

	for _, client := range clients {
		if _, ok := seen[client.ID]; ok {
			continue
		}
		// The state store does not persist the credential generation. An
		// offline client whose last reported generation matches the current
		// one cannot be confirmed as synced, so it is shown as unknown;
		// everything else is pending.
		status := "pending"
		if currentGeneration > 0 && client.CredentialGeneration == currentGeneration {
			status = "unknown"
		}
		out = append(out, serverui.ClientCredentialView{
			ClientID:   client.ID,
			Name:       client.Name,
			Online:     false,
			Generation: client.CredentialGeneration,
			Status:     status,
			LastSeen:   client.LastSeen,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClientID < out[j].ClientID })
	return out
}

func maskSecret(value string) string {
	if value == "" {
		return ""
	}
	return serverui.MaskedSecret
}

func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func boolLabel(value bool, whenTrue, whenFalse string) string {
	if value {
		return whenTrue
	}
	return whenFalse
}

// ConfigSummary returns a secret-free configuration summary for the web
// console's read-only settings page.
func (s *Server) ConfigSummary() serverui.ConfigSummary {
	cfg := s.currentConfig()
	webListen := cfg.Web.Listen
	if s.web != nil {
		webListen = s.web.Addr()
	}
	return serverui.ConfigSummary{
		ConfigPath:             cfg.ConfigPath,
		Listen:                 s.listenAddr.String(),
		ListenHost:             cfg.ListenHost,
		WebListen:              webListen,
		WebUsername:            cfg.Web.Username,
		WebTLS:                 cfg.Web.TLS.Cert != "",
		TLS:                    cfg.TLS.Cert != "",
		StateFile:              cfg.StateFile,
		PortsAllow:             append([]string(nil), cfg.Ports.Allow...),
		PortsDeny:              append([]string(nil), cfg.Ports.Deny...),
		PortsAutoRange:         cfg.Ports.AutoRange,
		OfflineReserve:         time.Duration(cfg.Ports.OfflineReserve),
		OfflineReserveSeconds:  int64(time.Duration(cfg.Ports.OfflineReserve) / time.Second),
		TrustProxy:             s.trustProxy.Load(),
		LogLevel:               cfg.Log.Level,
		Version:                version.Version,
		ControlTokenConfigured: cfg.Control.Token != "",
		WebPasswordConfigured:  cfg.Web.PasswordHash != "",
	}
}

// loadWebSessionKey loads the persisted web session signing key next to the
// configuration file. Configurations created programmatically without a path
// get an ephemeral key, which is safe but invalidates sessions on restart.
func loadWebSessionKey(cfg *config.ServerConfig, log *slog.Logger) ([]byte, error) {
	if cfg.ConfigPath == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate web session key: %w", err)
		}
		log.Warn("web session key is ephemeral because the config path is unknown")
		return key, nil
	}
	key, err := config.LoadOrCreateWebSecret(config.WebSecretPath(cfg.ConfigPath))
	if err != nil {
		return nil, fmt.Errorf("load web session key: %w", err)
	}
	return key, nil
}
