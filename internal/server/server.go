// Package server implements the hy2tund HY2 server. It serves standard
// Hysteria 2 TCP and UDP proxy traffic and, in later tasks, also hosts the
// hy2tun control stream and reverse data plane on reserved virtual addresses.
package server

import (
	"context"
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
	s.mappings = newMappingManager(policy, s.store, log, cfg.ListenHost, s.reservedPorts())
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

// WebAddr returns the configured web listener address. The web server itself
// is started by a later task.
func (s *Server) WebAddr() string {
	return s.cfg.Web.Listen
}
