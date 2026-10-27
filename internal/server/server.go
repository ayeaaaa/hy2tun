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
	"sync"
	"sync/atomic"
	"time"

	hyserver "github.com/apernet/hysteria/core/v2/server"
	"github.com/apernet/hysteria/extras/v2/auth"
	"github.com/apernet/hysteria/extras/v2/obfs"

	"hy2tun/internal/config"
	"hy2tun/internal/ports"
	"hy2tun/internal/state"
	"hy2tun/internal/version"
	"hy2tun/internal/webui/serverui"
)

// Server is a running hy2tund HY2 server.
type Server struct {
	cfg *config.ServerConfig
	log *slog.Logger

	hyServer   hyserver.Server
	packetConn net.PacketConn
	listenAddr net.Addr

	store    *state.Store
	mappings *mappingManager
	control  *controlHandler
	data     *dataStreams
	web      *serverui.Server

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
	authenticator, err := newAuthenticator(&cfg.Auth)
	if err != nil {
		return nil, err
	}

	packetConn, err := net.ListenPacket("udp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen UDP %s: %w", cfg.Listen, err)
	}

	s := &Server{
		cfg:        cfg,
		log:        log,
		packetConn: packetConn,
		listenAddr: packetConn.LocalAddr(),
		startedAt:  time.Now(),
		data:       newDataStreams(),
	}

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
func newAuthenticator(authCfg *config.AuthConfig) (hyserver.Authenticator, error) {
	switch authCfg.Type {
	case "password":
		return &auth.PasswordAuthenticator{Password: authCfg.Password}, nil
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
	return s.cfg.Web.Listen
}

// ConfigSummary returns a secret-free configuration summary for the web
// console's read-only settings page.
func (s *Server) ConfigSummary() serverui.ConfigSummary {
	webListen := s.cfg.Web.Listen
	if s.web != nil {
		webListen = s.web.Addr()
	}
	return serverui.ConfigSummary{
		ConfigPath:             s.cfg.ConfigPath,
		Listen:                 s.listenAddr.String(),
		ListenHost:             s.cfg.ListenHost,
		WebListen:              webListen,
		WebUsername:            s.cfg.Web.Username,
		WebTLS:                 s.cfg.Web.TLS.Cert != "",
		TLS:                    s.cfg.TLS.Cert != "",
		StateFile:              s.cfg.StateFile,
		PortsAllow:             append([]string(nil), s.cfg.Ports.Allow...),
		PortsDeny:              append([]string(nil), s.cfg.Ports.Deny...),
		PortsAutoRange:         s.cfg.Ports.AutoRange,
		OfflineReserve:         time.Duration(s.cfg.Ports.OfflineReserve),
		OfflineReserveSeconds:  int64(time.Duration(s.cfg.Ports.OfflineReserve) / time.Second),
		LogLevel:               s.cfg.Log.Level,
		Version:                version.Version,
		ControlTokenConfigured: s.cfg.Control.Token != "",
		WebPasswordConfigured:  s.cfg.Web.PasswordHash != "",
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
