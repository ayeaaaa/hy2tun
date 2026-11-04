package e2e

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	xproxy "golang.org/x/net/proxy"

	"hy2tun/internal/client"
	"hy2tun/internal/config"
	"hy2tun/internal/server"
	"hy2tun/internal/testutil"
)

// Real TLS/yamux listener, control HelloAck, reverse TCP/UDP and both local
// proxy protocols. No HY2 socket/listener is available to hide a wrong branch.
func TestForcedTCPMappingsAndLocalProxies(t *testing.T) {
	dir := t.TempDir()
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	certPath, keyPath := writeCertificate(t, dir, cert)
	tcpAddr, _ := testutil.StartTCPEcho(t)
	udpAddr, _ := testutil.StartUDPEcho(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := server.New(&config.ServerConfig{
		Mode: config.ModeTCP, Listen: "127.0.0.1:0",
		TLS:       config.TLSConfig{Cert: certPath, Key: keyPath},
		Auth:      config.AuthConfig{Type: "password", Password: e2ePassword},
		Control:   config.ControlConfig{Token: e2eControlToken},
		Ports:     config.PortsConfig{Allow: []string{"37000-37999"}, AutoRange: "37000-37999"},
		StateFile: filepath.Join(dir, "state.json"),
	}, log)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverDone := make(chan error, 1)
	go func() { serverDone <- srv.Start(ctx) }()
	defer func() { _ = srv.Close(); <-serverDone }()
	// Even a standard UDP dial to the configured server endpoint cannot work.
	udpBlock, err := net.ListenPacket("udp", srv.ListenAddr().String())
	require.NoError(t, err)
	defer udpBlock.Close()
	path := filepath.Join(dir, "client.yaml")
	cfg := &config.ClientConfig{
		Mode: config.ModeTCP, Server: srv.ListenAddr().String(), Auth: e2ePassword,
		TLS: config.ClientTLSConfig{CA: certPath}, Control: config.ControlConfig{Token: e2eControlToken},
		Client: config.ClientIdentity{ID: "forced-tcp", Name: "forced-tcp"},
		Proxy:  config.ProxyConfig{Enabled: true, SOCKS5: freeLocalTCPAddress(t), HTTP: freeLocalTCPAddress(t)},
		Mappings: []config.Mapping{
			{ID: "tcp", Type: "tcp", LocalAddr: tcpAddr, Enabled: true},
			{ID: "udp", Type: "udp", LocalAddr: udpAddr, Enabled: true},
		},
	}
	require.NoError(t, config.SaveClient(path, cfg))
	loaded, err := config.LoadClient(path)
	require.NoError(t, err)
	cl, err := client.New(loaded, log)
	require.NoError(t, err)
	clientDone := make(chan error, 1)
	go func() { clientDone <- cl.Start(ctx) }()
	defer func() { _ = cl.Close(); <-clientDone }()
	ready, stopReady := context.WithTimeout(ctx, 3*time.Second)
	defer stopReady()
	require.NoError(t, cl.WaitReady(ready))
	views := cl.Mappings()
	require.Len(t, views, 2)
	for _, m := range views {
		require.Equal(t, "running", m.Status)
		addr := fmt.Sprintf("127.0.0.1:%d", m.AssignedPort)
		if m.Type == "tcp" {
			require.NoError(t, tcpEcho(addr, "forced-tcp"))
		} else {
			require.Eventually(t, func() bool { return udpEcho(addr, "forced-udp") == nil }, 3*time.Second, 10*time.Millisecond)
		}
	}
	saved, err := config.LoadClient(path)
	require.NoError(t, err)
	require.Equal(t, config.ModeTCP, saved.Mode)
	require.Equal(t, int64(1), saved.Client.Revision)
	for _, m := range saved.Mappings {
		require.NotZero(t, m.RemotePort)
	}

	socks, err := xproxy.SOCKS5("tcp", cfg.Proxy.SOCKS5, nil, xproxy.Direct)
	require.NoError(t, err)
	socksConn, err := socks.Dial("tcp", tcpAddr)
	require.NoError(t, err)
	requireEchoConn(t, socksConn, "socks-over-tcp")
	_ = socksConn.Close()

	httpConn, err := net.DialTimeout("tcp", cfg.Proxy.HTTP, time.Second)
	require.NoError(t, err)
	defer httpConn.Close()
	_, err = fmt.Fprintf(httpConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", tcpAddr, tcpAddr)
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(httpConn), &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	requireEchoConn(t, httpConn, "http-over-tcp")
	require.Eventually(t, func() bool {
		m := cl.Mappings()
		return m[0].TxBytes > 0 && m[0].RxBytes > 0 && m[1].TxBytes > 0 && m[1].RxBytes > 0
	}, time.Second, time.Millisecond)
}

func freeLocalTCPAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func requireEchoConn(t *testing.T, conn net.Conn, message string) {
	t.Helper()
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	_, err := conn.Write([]byte(message))
	require.NoError(t, err)
	buf := make([]byte, len(message))
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, message, string(buf))
}
