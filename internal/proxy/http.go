package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// NewHTTP binds an HTTP CONNECT listener on listen. An optional auth value in
// "user:password" form enables HTTP Basic proxy authentication; an empty or
// absent value keeps the listener unauthenticated.
func NewHTTP(listen string, dial DialFunc, log *slog.Logger, auth ...string) (*Server, error) {
	srv, err := newServer(listen, dial, log, auth)
	if err != nil {
		return nil, err
	}
	srv.serve = srv.serveHTTP
	return srv, nil
}

// serveHTTP accepts a single CONNECT request, dials the target, and then
// relays bytes. Any other method is rejected with 405.
func (s *Server) serveHTTP(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(proxyHandshakeTimeout))
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		s.log.Debug("http proxy read request failed", "remote", conn.RemoteAddr(), "error", err)
		return
	}

	if req.Method != http.MethodConnect {
		_ = writeHTTPError(conn, http.StatusMethodNotAllowed, "")
		return
	}
	if s.auth != nil {
		user, pass, ok := proxyBasicAuth(req.Header.Get("Proxy-Authorization"))
		if !ok || !s.auth.matches(user, pass) {
			_ = writeHTTPError(conn, http.StatusProxyAuthRequired,
				"Proxy-Authenticate: Basic realm=\"hy2tun\"\r\n")
			return
		}
	}

	target := req.Host
	if target == "" {
		target = req.RequestURI
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		_ = writeHTTPError(conn, http.StatusBadRequest, "")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), proxyDialTimeout)
	remote, err := s.dial(ctx, "tcp", target)
	cancel()
	if err != nil {
		s.log.Debug("http proxy dial failed", "target", target, "error", err)
		_ = writeHTTPError(conn, http.StatusBadGateway, "")
		return
	}
	defer remote.Close()

	_ = conn.SetDeadline(time.Time{})
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	// http.ReadRequest may have buffered bytes that arrived with the CONNECT
	// request. Read the client side through the buffered reader so those bytes
	// reach the tunnel instead of being dropped.
	client := net.Conn(conn)
	if br.Buffered() > 0 {
		client = &bufferedConn{Conn: conn, r: br}
	}
	pipe(client, remote)
}

func writeHTTPError(w io.Writer, status int, extraHeaders string) error {
	_, err := fmt.Fprintf(w,
		"HTTP/1.1 %d %s\r\n%sContent-Length: 0\r\nConnection: close\r\n\r\n",
		status, http.StatusText(status), extraHeaders)
	return err
}

// proxyBasicAuth parses a Proxy-Authorization header. It accepts the Basic
// scheme and a base64 user:password value.
func proxyBasicAuth(header string) (user, pass string, ok bool) {
	scheme, value, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Basic") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return "", "", false
	}
	user, pass, ok = strings.Cut(string(decoded), ":")
	if !ok {
		return "", "", false
	}
	return user, pass, true
}

// bufferedConn reads through the buffered reader left by http.ReadRequest
// while delegating writes and connection metadata to the underlying conn.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}
