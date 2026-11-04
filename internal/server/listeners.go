package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	hyserver "github.com/apernet/hysteria/core/v2/server"
	"github.com/apernet/hysteria/extras/v2/obfs"
	"hy2tun/internal/config"
	"hy2tun/internal/firewall"
	"hy2tun/internal/transport"
)

type endpoint interface {
	Kind() transport.Kind
	Addr() net.Addr
	Serve(context.Context) error
	Close() error
	// markRetired records an intentional mode retirement so the Serve worker
	// does not report it as a fatal endpoint failure.
	markRetired()
	isRetired() bool
}

// prepareInitialEndpoints publishes the complete startup set, never a partial
// mode. UDP :0 selects independently of TCP, so an ephemeral dual pair may need
// bounded reselection. Fixed ports and later mode updates never reselect.
func (s *Server) prepareInitialEndpoints(cfg *config.ServerConfig, bindAddr string) error {
	ephemeralAddr, err := cfg.TCPListenAddress()
	if err != nil {
		return err
	}
	host, requestedPort, _ := net.SplitHostPort(ephemeralAddr)
	retryPair := cfg.Mode.Effective() == config.ModeAuto && requestedPort == "0"
	for attempt := 0; ; attempt++ {
		prepared := make(map[transport.Kind]endpoint)
		var primary net.Addr
		candidate := bindAddr
		var bindErr error
		for _, kind := range []transport.Kind{transport.HY2, transport.TCP} {
			if (kind == transport.HY2 && !cfg.Mode.AllowsHY2()) || (kind == transport.TCP && !cfg.Mode.AllowsTCP()) {
				continue
			}
			ep, err := s.prepareEndpoint(kind, cfg, candidate)
			if err != nil {
				bindErr = err
				break
			}
			prepared[kind] = ep
			if primary == nil {
				primary = ep.Addr()
				_, port, _ := net.SplitHostPort(primary.String())
				candidate = net.JoinHostPort(host, port)
			}
		}
		if bindErr != nil {
			for _, ep := range prepared {
				_ = ep.Close()
			}
			if retryPair && attempt < 15 && errors.Is(bindErr, syscall.EADDRINUSE) {
				bindAddr = ephemeralAddr
				continue
			}
			return bindErr
		}
		s.endpoints, s.listenAddr, s.baseBindAddr = prepared, primary, candidate
		for _, ep := range prepared {
			s.mappings.reserved[addrPort(ep.Addr())] = struct{}{}
		}
		if hy, ok := prepared[transport.HY2].(*hy2Endpoint); ok {
			s.hyServer, s.packetConn = hy.server, hy.packetConn
		}
		return nil
	}
}

// prepareEndpoint binds without accepting application traffic. Shared mappings,
// credentials and TLS must already exist. Each endpoint owns its own resources.
func (s *Server) prepareEndpoint(kind transport.Kind, cfg *config.ServerConfig, bindAddr string) (endpoint, error) {
	if kind == transport.TCP {
		listener, err := net.Listen("tcp", bindAddr)
		if err != nil {
			return nil, fmt.Errorf("listen TCP %s: %w", bindAddr, err)
		}
		return &tcpEndpoint{server: s, listener: listener, conns: make(map[net.Conn]bool)}, nil
	}
	if kind != transport.HY2 {
		return nil, fmt.Errorf("unknown endpoint kind %q", kind)
	}
	packetConn, err := net.ListenPacket("udp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("listen UDP %s: %w", bindAddr, err)
	}
	e := &hy2Endpoint{packetConn: packetConn}
	prepared := false
	defer func() {
		if !prepared {
			_ = e.Close()
		}
	}()
	ranges := cfg.ListenRanges
	needsRedirect := len(ranges) > 1 || (len(ranges) == 1 && ranges[0].Start != ranges[0].End)
	if needsRedirect {
		addr, ok := packetConn.LocalAddr().(*net.UDPAddr)
		if !ok {
			return nil, fmt.Errorf("unexpected UDP listen address %T", packetConn.LocalAddr())
		}
		cleanup, err := firewall.SetupUDPPortRedirect(addr, ranges)
		if err != nil {
			return nil, fmt.Errorf("set up port range redirects: %w", err)
		}
		e.cleanup = &onceCloser{closer: cleanup}
	}
	conn := packetConn
	if cfg.Obfs.Type != "" {
		if cfg.Obfs.Type != "salamander" {
			return nil, fmt.Errorf("unsupported obfs.type %q", cfg.Obfs.Type)
		}
		conn, err = obfs.WrapPacketConnSalamander(packetConn, []byte(cfg.Obfs.Salamander.Password))
		if err != nil {
			return nil, fmt.Errorf("configure salamander obfs: %w", err)
		}
	}
	authenticator, err := newAuthenticator(&cfg.Auth, s.credentials)
	if err != nil {
		return nil, err
	}
	e.server, err = hyserver.NewServer(&hyserver.Config{
		Cleanup:   e.cleanup,
		TLSConfig: hyserver.TLSConfig{Certificates: s.tlsConfig.Certificates},
		QUICConfig: hyserver.QUICConfig{
			InitialStreamReceiveWindow:     cfg.QUIC.InitialStreamReceiveWindow,
			MaxStreamReceiveWindow:         cfg.QUIC.MaxStreamReceiveWindow,
			InitialConnectionReceiveWindow: cfg.QUIC.InitialConnectionReceiveWindow,
			MaxConnectionReceiveWindow:     cfg.QUIC.MaxConnectionReceiveWindow,
			MaxIdleTimeout:                 time.Duration(cfg.QUIC.MaxIdleTimeout),
			MaxIncomingStreams:             cfg.QUIC.MaxIncomingStreams,
		},
		BandwidthConfig:       hyserver.BandwidthConfig{MaxTx: uint64(cfg.Bandwidth.Up), MaxRx: uint64(cfg.Bandwidth.Down)},
		IgnoreClientBandwidth: cfg.Bandwidth.IgnoreClient,
		Conn:                  conn, Outbound: &outbound{server: s, endpointClosed: &e.closed}, DisableUDP: false,
		Authenticator: authenticator, EventLogger: newEventLogger(s.log),
	})
	if err != nil {
		return nil, fmt.Errorf("create HY2 server: %w", err)
	}
	prepared = true
	return e, nil
}

// onceCloser makes cleanup safe across both upstream construction failure and
// local rollback. The HY2 implementation may close Cleanup itself on failure.
type onceCloser struct {
	closer io.Closer
	once   sync.Once
	err    error
}

func (c *onceCloser) Close() error {
	if c == nil {
		return nil
	}
	c.once.Do(func() {
		if c.closer != nil {
			c.err = c.closer.Close()
		}
	})
	return c.err
}

type hy2Endpoint struct {
	server     hyserver.Server
	packetConn net.PacketConn
	cleanup    *onceCloser
	closeOnce  sync.Once
	closeErr   error
	closed     atomic.Bool
	retired    atomic.Bool
}

func (e *hy2Endpoint) Kind() transport.Kind { return transport.HY2 }
func (e *hy2Endpoint) Addr() net.Addr       { return e.packetConn.LocalAddr() }
func (e *hy2Endpoint) markRetired()         { e.retired.Store(true) }
func (e *hy2Endpoint) isRetired() bool      { return e.retired.Load() }
func (e *hy2Endpoint) Serve(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { _ = e.Close() })
	defer stop()
	return e.server.Serve()
}
func (e *hy2Endpoint) Close() error {
	e.closeOnce.Do(func() {
		e.closed.Store(true)
		var errs []error
		if e.server != nil {
			errs = append(errs, e.server.Close())
		}
		if err := e.packetConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
		errs = append(errs, e.cleanup.Close())
		e.closeErr = errors.Join(errs...)
	})
	return e.closeErr
}

const (
	maxTCPHandshakes = 64
	maxTCPRoots      = 256
)

// tcpEndpoint inventories raw sockets throughout their lifetime, including
// authenticated roots without Hello. No unbounded handshake worker is launched.
type tcpEndpoint struct {
	server         *Server
	listener       net.Listener
	mu             sync.Mutex
	conns          map[net.Conn]bool // false: TLS/auth pending; true: authenticated root
	pending, roots int
	closed         bool
	cancel         context.CancelFunc
	workers        sync.WaitGroup
	closeOnce      sync.Once
	closeErr       error
	retired        atomic.Bool
}

func (e *tcpEndpoint) Kind() transport.Kind { return transport.TCP }
func (e *tcpEndpoint) Addr() net.Addr       { return e.listener.Addr() }
func (e *tcpEndpoint) markRetired()         { e.retired.Store(true) }
func (e *tcpEndpoint) isRetired() bool      { return e.retired.Load() }
func (e *tcpEndpoint) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return net.ErrClosed
	}
	e.cancel = cancel
	e.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = e.Close() })
	defer stop()
	for {
		conn, err := e.listener.Accept()
		if err != nil {
			return err
		}
		deadline := time.Now().Add(controlHandshakeTimeout)
		e.mu.Lock()
		if e.closed || e.pending >= maxTCPHandshakes {
			e.mu.Unlock()
			_ = conn.Close()
			continue
		}
		e.conns[conn] = false
		e.pending++
		e.workers.Add(1)
		e.mu.Unlock()
		go func() {
			defer e.workers.Done()
			defer e.release(conn)
			hctx := context.WithValue(ctx, tcpRegistrationDeadlineKey{}, deadline)
			hctx = context.WithValue(hctx, tcpAdmissionKey{}, func() error { return e.authenticate(conn) })
			_ = e.server.serveTCPConn(hctx, conn, e.server.tlsConfig)
		}()
	}
}

// authenticate reserves root capacity before the successful greeting and mux
// allocation. This callback is endpoint-local and never holds application locks.
func (e *tcpEndpoint) authenticate(conn net.Conn) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.roots >= maxTCPRoots {
		return transport.ErrAuthentication
	}
	if authenticated, ok := e.conns[conn]; !ok || authenticated {
		return transport.ErrAuthentication
	}
	e.conns[conn] = true
	e.pending--
	e.roots++
	return nil
}

func (e *tcpEndpoint) release(conn net.Conn) {
	e.mu.Lock()
	if authenticated, ok := e.conns[conn]; ok {
		delete(e.conns, conn)
		if authenticated {
			e.roots--
		} else {
			e.pending--
		}
	}
	e.mu.Unlock()
}

func (e *tcpEndpoint) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		cancel := e.cancel
		conns := make([]net.Conn, 0, len(e.conns))
		for conn := range e.conns {
			conns = append(conns, conn)
		}
		e.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if err := e.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			e.closeErr = err
		}
		for _, conn := range conns {
			_ = conn.Close()
		}
		e.workers.Wait()
	})
	return e.closeErr
}

// startEndpointLocked launches one endpoint's Serve worker at most once. Both
// Start and online mode activation use it, so an endpoint cannot be served
// twice when a mode update races startup. The caller holds listenersMu.
func (s *Server) startEndpointLocked(ep endpoint) {
	if s.serving == nil {
		s.serving = make(map[endpoint]struct{})
	}
	if _, ok := s.serving[ep]; ok {
		return
	}
	s.serving[ep] = struct{}{}
	s.log.Info("server listening", "transport", ep.Kind(), "listen", ep.Addr().String())
	go func() {
		err := ep.Serve(context.Background())
		s.retireEndpoint(ep)
		if ep.isRetired() {
			// An intentional mode retirement is not a fatal server error.
			return
		}
		select {
		case s.serveErr <- fmt.Errorf("serve %s: %w", ep.Kind(), err):
		default:
			s.log.Warn("endpoint serve error after shutdown", "transport", ep.Kind(), "error", err)
		}
	}()
}

// retireEndpoint never closes shared state/Web or unregisters a replacement.
// Task 8 serializes replacement publication with this retirement boundary.
func (s *Server) retireEndpoint(ep endpoint) {
	_ = ep.Close()
	s.listenersMu.Lock()
	delete(s.serving, ep)
	if s.endpoints[ep.Kind()] != ep {
		s.listenersMu.Unlock()
		return
	}
	delete(s.endpoints, ep.Kind())
	// Keep replacement publication ordered with transport session detachment.
	if s.mappings != nil {
		s.mappings.detachTransport(ep.Kind())
	}
	s.listenersMu.Unlock()
}

// ListenAddresses returns a fresh snapshot of currently owned endpoint sockets.
func (s *Server) ListenAddresses() map[transport.Kind]string {
	s.listenersMu.Lock()
	defer s.listenersMu.Unlock()
	addresses := make(map[transport.Kind]string, len(s.endpoints))
	for kind, ep := range s.endpoints {
		addresses[kind] = ep.Addr().String()
	}
	return addresses
}
