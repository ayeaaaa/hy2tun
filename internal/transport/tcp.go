package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatedier/yamux"
	"hy2tun/internal/config"
)

const handshakeTimeout = 10 * time.Second
const maxTCPStreams = 1024

// lookupTCPHost is a test seam for the TCP dialer's address fallback. It keeps
// the production resolver behavior while allowing a deterministic two-address
// test without changing /etc/hosts or public configuration.
var lookupTCPHost = net.DefaultResolver.LookupIPAddr

type Principal struct {
	ClientID    string
	Generation  int64
	TokenDigest [32]byte
}

// Authenticate must honor ctx. The callback owns current/previous credential
// policy; the adapter retains only the returned non-replayable principal.
type TCPAuthFunc func(context.Context, net.Addr, Greeting) (Principal, error)

type RateLimits struct{ Up, Down uint64 }

type TCPServerOptions struct {
	TLS          *tls.Config
	Authenticate TCPAuthFunc
	Mode         config.TransportMode
	Version      string
	Limits       RateLimits
}

type tcpSession struct {
	// admissionMu orders server-side admission commits with root invalidation.
	// It never protects stream I/O or a wait for an upstream operation.
	admissionMu           sync.Mutex
	mux                   *yamux.Session
	ctx                   context.Context
	cancel                context.CancelFunc
	slots                 chan struct{}
	readLimit, writeLimit *byteLimiter
	info                  ProbeInfo
	localClosed           atomic.Bool
}

type TCPPeer struct {
	session   *tcpSession
	principal Principal
}

// DialTCP owns the connection and uses a single TLS+greeting budget, capped at
// ten seconds. Cloning preserves verification callbacks, CA, SNI and explicit
// insecure policy; no TLS error triggers a retry with weaker security.
func DialTCP(ctx context.Context, opts DialOptions) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.TCPAddress == "" || opts.TLS == nil {
		return nil, errConfig
	}
	greeting := Greeting{Magic: protocolMagic, Version: protocolVersion, Auth: opts.Auth, ControlToken: opts.ControlToken, ClientID: opts.ClientID, Up: opts.Up, Down: opts.Down}
	if !validGreeting(greeting) {
		return nil, ErrProtocol
	}
	cfg, err := tcpTLS(opts.TLS)
	if err != nil {
		return nil, err
	}
	if cfg.ServerName == "" {
		host, _, e := net.SplitHostPort(opts.TCPAddress)
		if e != nil {
			return nil, errConfig
		}
		cfg.ServerName = host
	}

	addresses, err := resolveTCPAddresses(ctx, opts.TCPAddress)
	if err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	// A dual-stack hostname can resolve to an address whose TCP port accepts
	// the connection but resets TLS (seen with broken IPv6 paths). A plain
	// net.Dialer only falls back before connect, so try each address here and
	// preserve the same TLS policy for every attempt. Security/auth/protocol
	// failures stop immediately; only recognized network failures fall through.
	perAddress := handshakeTimeout
	if len(addresses) > 1 {
		perAddress = handshakeTimeout / time.Duration(len(addresses))
		if perAddress < time.Second {
			perAddress = time.Second
		}
	}
	var lastErr error
	for _, address := range addresses {
		attemptCtx, attemptCancel := context.WithTimeout(hctx, perAddress)
		session, err := dialTCPAddress(attemptCtx, opts, greeting, cfg, address)
		attemptCancel()
		if err == nil {
			return session, nil
		}
		lastErr = err
		if !FallbackAllowed(err) {
			return nil, err
		}
		if hctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = errConfig
	}
	return nil, lastErr
}

func resolveTCPAddresses(ctx context.Context, address string) ([]string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errConfig
	}
	if ip := net.ParseIP(host); ip != nil {
		return []string{net.JoinHostPort(host, port)}, nil
	}
	ips, err := lookupTCPHost(ctx, host)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(ips))
	out := make([]string, 0, len(ips))
	for _, candidate := range ips {
		if candidate.IP == nil {
			continue
		}
		host := candidate.IP.String()
		if candidate.Zone != "" {
			host += "%" + candidate.Zone
		}
		address := net.JoinHostPort(host, port)
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		out = append(out, address)
	}
	if len(out) == 0 {
		return nil, errConfig
	}
	return out, nil
}

func dialTCPAddress(ctx context.Context, opts DialOptions, greeting Greeting, cfg *tls.Config, address string) (Session, error) {
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, cfg)
	ok := false
	defer func() {
		if !ok {
			_ = raw.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		_ = conn.SetDeadline(deadline)
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		return nil, tlsHandshakeError(ctx, err)
	}
	if err := writeJSONFrame(conn, greeting, greetingLimit); err != nil {
		return nil, contextError(ctx, err)
	}
	var ack GreetingAck
	if err := readJSONFrame(conn, &ack, greetingLimit); err != nil {
		return nil, contextError(ctx, err)
	}
	if !ack.OK {
		if !safeWireError(ack.Error) {
			return nil, ErrProtocol
		}
		if ack.Error == "unauthorized" {
			return nil, ErrAuthentication
		}
		return nil, ErrProtocol
	}
	if ack.Version != protocolVersion || ack.Error != "" || !validWireMode(ack.Mode) {
		return nil, ErrProtocol
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !stop() {
		return nil, contextError(ctx, net.ErrClosed)
	}
	_ = conn.SetDeadline(time.Time{})
	mux, err := yamux.Client(conn, tcpMuxConfig())
	if err != nil {
		return nil, ErrProtocol
	}
	s := newTCPSession(mux, opts.Down, opts.Up)
	s.info = ProbeInfo{Version: ack.Version, Mode: ack.Mode, ServerVersion: ack.ServerVersion}
	ok = true
	return s, nil
}

func AcceptTCP(ctx context.Context, raw net.Conn, opts TCPServerOptions) (*TCPPeer, error) {
	if raw == nil {
		return nil, errConfig
	}
	ok := false
	defer func() {
		if !ok {
			_ = raw.Close()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.TLS == nil || opts.Authenticate == nil || !validWireMode(opts.Mode) || len(opts.Version) > greetingLimit {
		return nil, errConfig
	}
	cfg, err := tcpTLS(opts.TLS)
	if err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	stop := context.AfterFunc(hctx, func() { _ = raw.Close() })
	defer stop()
	conn := tls.Server(raw, cfg)
	deadline, _ := hctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if err := conn.HandshakeContext(hctx); err != nil {
		return nil, tlsHandshakeError(hctx, err)
	}
	var greeting Greeting
	err = readJSONFrame(conn, &greeting, greetingLimit)
	if err != nil || !validGreeting(greeting) {
		code := "bad_message"
		if err == nil && (greeting.Magic != protocolMagic || greeting.Version != protocolVersion) {
			code = "unsupported_protocol"
		}
		_ = writeJSONFrame(conn, GreetingAck{Version: protocolVersion, Error: code}, greetingLimit)
		return nil, contextError(hctx, ErrProtocol)
	}
	principal, err := opts.Authenticate(hctx, raw.RemoteAddr(), greeting)
	if err != nil {
		_ = writeJSONFrame(conn, GreetingAck{Version: protocolVersion, Error: "unauthorized"}, greetingLimit)
		return nil, contextError(hctx, ErrAuthentication)
	}
	if err := hctx.Err(); err != nil {
		return nil, err
	}
	ack := GreetingAck{OK: true, Version: protocolVersion, Mode: opts.Mode, ServerVersion: opts.Version}
	if err := writeJSONFrame(conn, ack, greetingLimit); err != nil {
		return nil, contextError(hctx, err)
	}
	if !stop() {
		return nil, contextError(hctx, net.ErrClosed)
	}
	_ = conn.SetDeadline(time.Time{})
	// Authentication and its reply complete before the mux reads any bytes.
	mux, err := yamux.Server(conn, tcpMuxConfig())
	if err != nil {
		return nil, ErrProtocol
	}
	// Server Up means client -> server, Down server -> client. The client's
	// limiter and this server limiter are separate, never silently negotiated away.
	s := newTCPSession(mux, opts.Limits.Up, opts.Limits.Down)
	ok = true
	return &TCPPeer{session: s, principal: principal}, nil
}

func validGreeting(g Greeting) bool {
	return g.Magic == protocolMagic && g.Version == protocolVersion && g.Auth != "" && g.ControlToken != "" && g.ClientID != "" && len(g.Auth)+len(g.ControlToken)+len(g.ClientID) <= greetingLimit
}

func tcpTLS(original *tls.Config) (*tls.Config, error) {
	cfg := original.Clone()
	if cfg.MaxVersion != 0 && cfg.MaxVersion < tls.VersionTLS13 {
		return nil, errTLS
	}
	if cfg.MinVersion < tls.VersionTLS13 {
		cfg.MinVersion = tls.VersionTLS13
	}
	// TLS can replace the entire server config after ClientHello. Constrain
	// dynamically selected configs too, without changing certificate selection
	// or mutating any caller-owned config.
	if selectConfig := cfg.GetConfigForClient; selectConfig != nil {
		cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			selected, err := selectConfig(hello)
			if err != nil || selected == nil {
				return selected, err
			}
			return tcpTLS(selected)
		}
	}
	return cfg, nil
}

func tlsHandshakeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Certificate and pin sentinels retain identity; classify otherwise textual
	// TLS alerts as TLS failures rather than permitting insecure failover.
	var network net.Error
	var opError *net.OpError
	if errors.As(err, &opError) || (errors.As(err, &network) && network.Timeout()) {
		return err
	}
	if errors.Is(err, io.EOF) {
		return err
	}
	return &Error{Code: errTLS.Code, Cause: err}
}

func contextError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func tcpMuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.AcceptBacklog = 256
	c.MaxStreamWindowSize = 1 << 20
	c.StreamOpenTimeout = handshakeTimeout
	c.ConnectionWriteTimeout = handshakeTimeout
	c.KeepAliveInterval = handshakeTimeout
	c.StreamCloseTimeout = handshakeTimeout
	c.LogOutput = io.Discard // No peer-controlled headers/targets in transport logs.
	return c
}

func newTCPSession(mux *yamux.Session, read, write uint64) *tcpSession {
	ctx, cancel := context.WithCancel(context.Background())
	s := &tcpSession{mux: mux, ctx: ctx, cancel: cancel, slots: make(chan struct{}, maxTCPStreams), readLimit: newByteLimiter(read), writeLimit: newByteLimiter(write)}
	go func() {
		<-mux.CloseChan()
		s.admissionMu.Lock()
		cancel()
		s.admissionMu.Unlock()
	}()
	return s
}

func (s *tcpSession) Kind() Kind { return TCP }
func (s *tcpSession) Close() error {
	s.admissionMu.Lock()
	s.localClosed.Store(true)
	s.cancel()
	s.admissionMu.Unlock()
	return s.mux.Close()
}
func (p *TCPPeer) Principal() Principal { return p.principal }
func (p *TCPPeer) Close() error         { return p.session.Close() }

// WithLive executes a short, nonblocking admission commit only if the root is
// still live. It serializes the commit with local Close and observed remote mux
// shutdown; Close cannot finish invalidation midway through the commit. A remote
// shutdown already visible on the mux also prevents callback execution.
//
// Callers must acquire their own state/configuration locks before calling this
// method. The callback must not call Close/WithLive on this same root, perform
// network/file I/O, or wait for child draining. A successful commit can naturally
// be followed by root closure; this is not a lease on subsequent business I/O.
// Closed-root rejection returns net.ErrClosed without consuming queued streams
// or classifying the underlying I/O failure. Ordinary session/stream I/O retains
// failure classification outside this admission guard.
func (p *TCPPeer) WithLive(commit func() error) error {
	if p == nil || p.session == nil || commit == nil {
		return errConfig
	}
	s := p.session
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	if s.localClosed.Load() || s.ctx.Err() != nil || s.mux.IsClosed() {
		return net.ErrClosed
	}
	return commit()
}
func (s *tcpSession) Probe(ctx context.Context) (ProbeInfo, error) {
	if err := ctx.Err(); err != nil {
		return ProbeInfo{}, err
	}
	if s.ctx.Err() != nil || s.mux.IsClosed() {
		return ProbeInfo{}, s.connectionError(net.ErrClosed)
	}
	return s.info, nil
}

// A cancelled internal lifecycle context is not necessarily caller
// cancellation: the remote mux may have failed. Retain its actual cause for
// the existing fallback classifier, without turning local shutdown or malformed
// yamux into an allowed network failure.
func (s *tcpSession) connectionError(err error) error {
	if err == nil || errors.Is(err, ErrAuthentication) || errors.Is(err, ErrProtocol) {
		return err
	}
	if s.localClosed.Load() {
		return net.ErrClosed
	}
	if !s.mux.IsClosed() {
		return err
	}
	// Obtain the recorded shutdown cause on ordinary I/O error paths. AcceptStream
	// can wait for physical teardown when it selects a queued stream during
	// shutdown, so this classifier must not run under application admission locks.
	_, cause := s.mux.AcceptStream()
	for _, protocolError := range []error{yamux.ErrInvalidVersion, yamux.ErrInvalidMsgType, yamux.ErrStreamsExhausted, yamux.ErrDuplicateStream, yamux.ErrRecvWindowExceeded, yamux.ErrUnexpectedFlag} {
		if errors.Is(cause, protocolError) {
			return &Error{Code: ErrProtocol.Code, Cause: cause}
		}
	}
	if errors.Is(cause, yamux.ErrKeepAliveTimeout) || errors.Is(cause, yamux.ErrConnectionWriteTimeout) {
		return errors.Join(cause, context.DeadlineExceeded)
	}
	if cause == nil || errors.Is(cause, yamux.ErrSessionShutdown) {
		return io.EOF
	}
	return cause
}
