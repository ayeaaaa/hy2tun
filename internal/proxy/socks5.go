// Package proxy implements the optional local SOCKS5 and HTTP CONNECT proxy
// listeners. Connections accepted by a listener are tunnelled through the
// client's HY2 connection by the caller-supplied DialFunc.
package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DialFunc opens a tunnelled TCP connection to addr. The network is always
// "tcp"; it is part of the signature so a caller can pass a dialer that already
// has that shape.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

const (
	proxyDialTimeout      = 10 * time.Second
	proxyHandshakeTimeout = 10 * time.Second

	socks5Version     = 0x05
	socks5AuthVersion = 0x01

	socks5MethodNoAuth       = 0x00
	socks5MethodUserPassword = 0x02
	socks5MethodNoneAccepted = 0xff

	socks5CommandConnect      = 0x01
	socks5CommandBind         = 0x02
	socks5CommandUDPAssociate = 0x03

	socks5AddressIPv4   = 0x01
	socks5AddressDomain = 0x03
	socks5AddressIPv6   = 0x04

	socks5ReplySuccess             = 0x00
	socks5ReplyGeneralFailure      = 0x01
	socks5ReplyHostUnreachable     = 0x04
	socks5ReplyCommandNotSupported = 0x07
	socks5ReplyAddressNotSupported = 0x08
	socks5AuthReplySuccess         = 0x00
	socks5AuthReplyFailure         = 0x01
)

// Server is one bound proxy listener. The TCP listener is created by the
// constructor, so Addr is valid before Start.
type Server struct {
	ln     net.Listener
	dial   DialFunc
	log    *slog.Logger
	auth   *credentials
	serve  func(net.Conn)
	ctx    context.Context
	cancel context.CancelFunc

	closed    atomic.Bool
	closeOnce sync.Once
	wg        sync.WaitGroup
	mu        sync.Mutex
	conns     map[net.Conn]struct{}
}

type credentials struct {
	user string
	pass string
}

// NewSOCKS5 binds a SOCKS5 listener on listen. An optional auth value in
// "user:password" form enables RFC 1929 username/password authentication; an
// empty or absent value keeps the listener unauthenticated.
func NewSOCKS5(listen string, dial DialFunc, log *slog.Logger, auth ...string) (*Server, error) {
	srv, err := newServer(listen, dial, log, auth)
	if err != nil {
		return nil, err
	}
	srv.serve = srv.serveSOCKS5
	return srv, nil
}

func newServer(listen string, dial DialFunc, log *slog.Logger, auth []string) (*Server, error) {
	if dial == nil {
		return nil, fmt.Errorf("proxy dial function is required")
	}
	if log == nil {
		log = slog.Default()
	}
	creds, err := parseCredentials(auth)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("listen proxy on %s: %w", listen, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		ln:     ln,
		dial:   dial,
		log:    log,
		auth:   creds,
		ctx:    ctx,
		cancel: cancel,
		conns:  make(map[net.Conn]struct{}),
	}, nil
}

// parseCredentials accepts zero or one "user:password" value. An empty value
// means no authentication.
func parseCredentials(values []string) (*credentials, error) {
	if len(values) > 1 {
		return nil, fmt.Errorf("proxy auth accepts at most one value")
	}
	if len(values) == 0 || values[0] == "" {
		return nil, nil
	}
	user, pass, ok := strings.Cut(values[0], ":")
	if !ok || user == "" {
		return nil, fmt.Errorf("proxy auth must be in user:password form")
	}
	if pass == "" {
		return nil, fmt.Errorf("proxy auth must include a password")
	}
	return &credentials{user: user, pass: pass}, nil
}

func (c *credentials) matches(user, pass string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(c.user)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(c.pass)) == 1
	return userOK && passOK
}

// Addr returns the bound listener address.
func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

// Start accepts and serves connections until Close is called. It blocks.
func (s *Server) Start() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.closed.Load() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept proxy connection: %w", err)
		}
		if !s.track(conn) {
			_ = conn.Close()
			continue
		}
		go func() {
			defer s.wg.Done()
			defer s.untrack(conn)
			defer conn.Close()
			s.serve(conn)
		}()
	}
}

// Close stops the listener, closes active proxy connections, and waits for
// their handlers. It is idempotent.
func (s *Server) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if s.cancel != nil {
			s.cancel()
		}
		closeErr = s.ln.Close()

		s.mu.Lock()
		conns := make([]net.Conn, 0, len(s.conns))
		for conn := range s.conns {
			conns = append(conns, conn)
		}
		s.conns = make(map[net.Conn]struct{})
		s.mu.Unlock()

		for _, conn := range conns {
			_ = conn.Close()
		}
		s.wg.Wait()
	})
	return closeErr
}

// track registers an accepted connection. The wait-group count is incremented
// under the same lock so Close cannot observe a zero count and return while a
// handler is about to start.
func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return false
	}
	s.conns[conn] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

// serveSOCKS5 performs the SOCKS5 greeting, optional authentication, and a
// single CONNECT request. BIND and UDP ASSOCIATE are rejected in v1.
func (s *Server) serveSOCKS5(conn net.Conn) {
	_ = conn.SetDeadline(time.Now().Add(proxyHandshakeTimeout))
	target, err := s.socks5Handshake(conn)
	if err != nil {
		s.log.Debug("socks5 handshake failed", "remote", conn.RemoteAddr(), "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, proxyDialTimeout)
	remote, err := s.dial(ctx, "tcp", target)
	cancel()
	if err != nil {
		s.log.Debug("socks5 dial failed", "target", target, "error", err)
		_ = writeSOCKS5Reply(conn, socks5ReplyForError(err), nil)
		return
	}
	defer remote.Close()

	if err := writeSOCKS5Reply(conn, socks5ReplySuccess, remote.LocalAddr()); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	pipe(conn, remote)
}

func (s *Server) socks5Handshake(conn net.Conn) (string, error) {
	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		return "", fmt.Errorf("read greeting: %w", err)
	}
	if greeting[0] != socks5Version {
		return "", fmt.Errorf("unsupported SOCKS version %d", greeting[0])
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return "", fmt.Errorf("read auth methods: %w", err)
	}

	method := byte(socks5MethodNoAuth)
	if s.auth != nil {
		method = socks5MethodUserPassword
	}
	if !containsByte(methods, method) {
		_, _ = conn.Write([]byte{socks5Version, socks5MethodNoneAccepted})
		return "", fmt.Errorf("no acceptable authentication method")
	}
	if _, err := conn.Write([]byte{socks5Version, method}); err != nil {
		return "", fmt.Errorf("write auth method: %w", err)
	}
	if s.auth != nil {
		if err := s.socks5Authenticate(conn); err != nil {
			return "", err
		}
	}

	var request [4]byte
	if _, err := io.ReadFull(conn, request[:]); err != nil {
		return "", fmt.Errorf("read request: %w", err)
	}
	if request[0] != socks5Version {
		return "", fmt.Errorf("unsupported request version %d", request[0])
	}
	if request[2] != 0 {
		return "", fmt.Errorf("invalid reserved byte %d", request[2])
	}
	switch request[1] {
	case socks5CommandConnect:
	case socks5CommandBind, socks5CommandUDPAssociate:
		_ = writeSOCKS5Reply(conn, socks5ReplyCommandNotSupported, nil)
		return "", fmt.Errorf("command %d is not supported in v1", request[1])
	default:
		_ = writeSOCKS5Reply(conn, socks5ReplyCommandNotSupported, nil)
		return "", fmt.Errorf("unsupported command %d", request[1])
	}

	host, err := readSOCKS5Address(conn, request[3])
	if err != nil {
		_ = writeSOCKS5Reply(conn, socks5ReplyAddressNotSupported, nil)
		return "", err
	}
	var portBytes [2]byte
	if _, err := io.ReadFull(conn, portBytes[:]); err != nil {
		return "", fmt.Errorf("read port: %w", err)
	}
	port := binary.BigEndian.Uint16(portBytes[:])
	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

func (s *Server) socks5Authenticate(conn net.Conn) error {
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return fmt.Errorf("read auth header: %w", err)
	}
	if header[0] != socks5AuthVersion {
		return fmt.Errorf("unsupported auth version %d", header[0])
	}
	user := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, user); err != nil {
		return fmt.Errorf("read auth username: %w", err)
	}
	var passLen [1]byte
	if _, err := io.ReadFull(conn, passLen[:]); err != nil {
		return fmt.Errorf("read auth password length: %w", err)
	}
	pass := make([]byte, int(passLen[0]))
	if _, err := io.ReadFull(conn, pass); err != nil {
		return fmt.Errorf("read auth password: %w", err)
	}

	if !s.auth.matches(string(user), string(pass)) {
		_, _ = conn.Write([]byte{socks5AuthVersion, socks5AuthReplyFailure})
		return fmt.Errorf("invalid proxy credentials")
	}
	if _, err := conn.Write([]byte{socks5AuthVersion, socks5AuthReplySuccess}); err != nil {
		return fmt.Errorf("write auth reply: %w", err)
	}
	return nil
}

func readSOCKS5Address(r io.Reader, atyp byte) (string, error) {
	switch atyp {
	case socks5AddressIPv4:
		var ip [4]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return "", fmt.Errorf("read IPv4 address: %w", err)
		}
		return net.IP(ip[:]).String(), nil
	case socks5AddressIPv6:
		var ip [16]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return "", fmt.Errorf("read IPv6 address: %w", err)
		}
		return net.IP(ip[:]).String(), nil
	case socks5AddressDomain:
		var length [1]byte
		if _, err := io.ReadFull(r, length[:]); err != nil {
			return "", fmt.Errorf("read domain length: %w", err)
		}
		if length[0] == 0 {
			return "", fmt.Errorf("empty domain name")
		}
		domain := make([]byte, int(length[0]))
		if _, err := io.ReadFull(r, domain); err != nil {
			return "", fmt.Errorf("read domain name: %w", err)
		}
		return string(domain), nil
	default:
		return "", fmt.Errorf("unsupported address type %d", atyp)
	}
}

func writeSOCKS5Reply(w io.Writer, reply byte, addr net.Addr) error {
	host := net.IPv4zero
	port := 0
	if tcpAddr, ok := addr.(*net.TCPAddr); ok && tcpAddr != nil {
		host = tcpAddr.IP
		port = tcpAddr.Port
	}

	out := []byte{socks5Version, reply, 0x00}
	if ip4 := host.To4(); ip4 != nil {
		out = append(out, socks5AddressIPv4)
		out = append(out, ip4...)
	} else if ip16 := host.To16(); ip16 != nil {
		out = append(out, socks5AddressIPv6)
		out = append(out, ip16...)
	} else {
		out = append(out, socks5AddressIPv4, 0, 0, 0, 0)
	}
	out = append(out, byte(port>>8), byte(port))
	_, err := w.Write(out)
	return err
}

func socks5ReplyForError(err error) byte {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return socks5ReplyHostUnreachable
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return socks5ReplyHostUnreachable
	}
	return socks5ReplyGeneralFailure
}

func containsByte(values []byte, want byte) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// pipe copies data in both directions and closes both connections when either
// direction finishes, which releases the other copy.
func pipe(a, b net.Conn) {
	done := make(chan struct{}, 2)
	copyConn := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go copyConn(a, b)
	go copyConn(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}
