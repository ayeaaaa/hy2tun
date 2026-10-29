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
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	xproxy "golang.org/x/net/proxy"

	"hy2tun/internal/testutil"
)

// directDial is the test DialFunc: it connects straight to the target instead
// of through HY2.
func directDial(ctx context.Context, network, addr string) (net.Conn, error) {
	return net.Dial(network, addr)
}

func TestSOCKS5Connect(t *testing.T) {
	echoAddr, stop := testutil.StartTCPEcho(t)
	defer stop()

	srv, err := NewSOCKS5("127.0.0.1:0", directDial, slog.Default())
	require.NoError(t, err)
	go srv.Start()
	defer srv.Close()

	p, err := xproxy.SOCKS5("tcp", srv.Addr(), nil, xproxy.Direct)
	require.NoError(t, err)
	conn, err := p.Dial("tcp", echoAddr)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf))
}

func TestHTTPConnect(t *testing.T) {
	echoAddr, stop := testutil.StartTCPEcho(t)
	defer stop()

	srv, err := NewHTTP("127.0.0.1:0", directDial, slog.Default())
	require.NoError(t, err)
	go srv.Start()
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Addr())
	require.NoError(t, err)
	defer conn.Close()

	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	require.NoError(t, err)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = io.ReadFull(br, buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf))
}

func TestSOCKS5UsernamePasswordAuth(t *testing.T) {
	echoAddr, stop := testutil.StartTCPEcho(t)
	defer stop()

	srv, err := NewSOCKS5("127.0.0.1:0", directDial, slog.Default(), "alice:s3cret")
	require.NoError(t, err)
	go srv.Start()
	defer srv.Close()

	p, err := xproxy.SOCKS5("tcp", srv.Addr(), &xproxy.Auth{
		User:     "alice",
		Password: "s3cret",
	}, xproxy.Direct)
	require.NoError(t, err)
	conn, err := p.Dial("tcp", echoAddr)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf))

	bad, err := xproxy.SOCKS5("tcp", srv.Addr(), &xproxy.Auth{
		User:     "alice",
		Password: "wrong",
	}, xproxy.Direct)
	require.NoError(t, err)
	_, err = bad.Dial("tcp", echoAddr)
	require.Error(t, err)
}

func TestSOCKS5RejectsBindAndUDPAssociate(t *testing.T) {
	srv, err := NewSOCKS5("127.0.0.1:0", directDial, slog.Default())
	require.NoError(t, err)
	go srv.Start()
	defer srv.Close()

	for _, command := range []byte{socks5CommandBind, socks5CommandUDPAssociate} {
		conn := socks5Negotiate(t, srv.Addr())
		// CONNECT-shaped request with the unsupported command, IPv4
		// 127.0.0.1:9.
		_, err := conn.Write([]byte{5, command, 0, 1, 127, 0, 0, 1, 0, 9})
		require.NoError(t, err)
		reply := readSOCKS5Reply(t, conn)
		require.Equal(t, byte(0x07), reply, "command %d must be rejected", command)
		_ = conn.Close()
	}
}

func TestSOCKS5DomainAddress(t *testing.T) {
	echoAddr, stop := testutil.StartTCPEcho(t)
	defer stop()
	_, portText, err := net.SplitHostPort(echoAddr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)

	srv, err := NewSOCKS5("127.0.0.1:0", directDial, slog.Default())
	require.NoError(t, err)
	go srv.Start()
	defer srv.Close()

	conn := socks5Negotiate(t, srv.Addr())
	defer conn.Close()

	request := []byte{5, 1, 0, 3, byte(len("localhost"))}
	request = append(request, []byte("localhost")...)
	request = append(request, byte(port>>8), byte(port))
	_, err = conn.Write(request)
	require.NoError(t, err)
	require.Equal(t, byte(0x00), readSOCKS5Reply(t, conn))

	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf))
}

func TestHTTPRejectsNonConnect(t *testing.T) {
	srv, err := NewHTTP("127.0.0.1:0", directDial, slog.Default())
	require.NoError(t, err)
	go srv.Start()
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Addr())
	require.NoError(t, err)
	defer conn.Close()

	_, err = io.WriteString(conn, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestHTTPBasicAuth(t *testing.T) {
	echoAddr, stop := testutil.StartTCPEcho(t)
	defer stop()

	srv, err := NewHTTP("127.0.0.1:0", directDial, slog.Default(), "bob:hunter2")
	require.NoError(t, err)
	go srv.Start()
	defer srv.Close()

	// Without credentials the proxy challenges the client.
	conn, err := net.Dial("tcp", srv.Addr())
	require.NoError(t, err)
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echoAddr, echoAddr)
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	require.Equal(t, http.StatusProxyAuthRequired, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Proxy-Authenticate"), "Basic")
	_ = resp.Body.Close()
	_ = conn.Close()

	// Correct credentials open the tunnel.
	conn, err = net.Dial("tcp", srv.Addr())
	require.NoError(t, err)
	defer conn.Close()
	header := base64.StdEncoding.EncodeToString([]byte("bob:hunter2"))
	_, err = fmt.Fprintf(conn,
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n",
		echoAddr, echoAddr, header)
	require.NoError(t, err)
	br := bufio.NewReader(conn)
	resp, err = http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = io.ReadFull(br, buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf))
}

func TestProxyAuthValidation(t *testing.T) {
	_, err := NewSOCKS5("127.0.0.1:0", directDial, slog.Default(), "missing-colon")
	require.Error(t, err)
	_, err = NewHTTP("127.0.0.1:0", directDial, slog.Default(), "user:")
	require.Error(t, err)
	_, err = NewHTTP("127.0.0.1:0", directDial, slog.Default(), "a:b", "c:d")
	require.Error(t, err)
}

// socks5Negotiate opens a SOCKS5 connection and completes the no-auth method
// negotiation.
func socks5Negotiate(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	_, err = conn.Write([]byte{5, 1, 0})
	require.NoError(t, err)
	var reply [2]byte
	_, err = io.ReadFull(conn, reply[:])
	require.NoError(t, err)
	require.Equal(t, []byte{5, 0}, reply[:])
	return conn
}

// readSOCKS5Reply reads a full SOCKS5 reply and returns its status byte.
func readSOCKS5Reply(t *testing.T, conn net.Conn) byte {
	t.Helper()
	var head [4]byte
	_, err := io.ReadFull(conn, head[:])
	require.NoError(t, err)
	require.Equal(t, byte(5), head[0])

	var addressLen int
	switch head[3] {
	case 1:
		addressLen = 4
	case 4:
		addressLen = 16
	case 3:
		var length [1]byte
		_, err = io.ReadFull(conn, length[:])
		require.NoError(t, err)
		addressLen = int(length[0])
	default:
		t.Fatalf("unexpected address type %d", head[3])
	}
	_, err = io.ReadFull(conn, make([]byte, addressLen+2))
	require.NoError(t, err)
	return head[1]
}

func TestCloseUnblocksActiveTunnel(t *testing.T) {
	echoAddr, stop := testutil.StartTCPEcho(t)
	defer stop()

	srv, err := NewSOCKS5("127.0.0.1:0", directDial, slog.Default())
	require.NoError(t, err)
	go srv.Start()

	p, err := xproxy.SOCKS5("tcp", srv.Addr(), nil, xproxy.Direct)
	require.NoError(t, err)
	conn, err := p.Dial("tcp", echoAddr)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- srv.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not stop the active tunnel")
	}
}

func TestCloseCancelsBlockedDial(t *testing.T) {
	dialStarted := make(chan struct{})
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(dialStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	srv, err := NewSOCKS5("127.0.0.1:0", dial, slog.Default())
	require.NoError(t, err)
	go srv.Start()

	conn, err := net.Dial("tcp", srv.Addr())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte{0x05, 0x01, 0x00})
	require.NoError(t, err)
	reply := make([]byte, 2)
	_, err = io.ReadFull(conn, reply)
	require.NoError(t, err)
	_, err = conn.Write([]byte{0x05, 0x01, 0x00, 0x01, 1, 1, 1, 1, 0, 80})
	require.NoError(t, err)
	select {
	case <-dialStarted:
	case <-time.After(time.Second):
		t.Fatal("dial was not started")
	}

	done := make(chan error, 1)
	go func() { done <- srv.Close() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not cancel blocked dial")
	}
}
