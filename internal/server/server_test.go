package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/extras/v2/obfs"
	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/testutil"
)

// testTempDir holds certificate files written by validServerConfig. It is
// created once per test binary and removed by TestMain.
var testTempDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hy2tun-server-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create test temp dir: %v\n", err)
		os.Exit(1)
	}
	testTempDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// validServerConfig returns a minimal valid server configuration whose TLS
// certificate and key are written to temporary files.
func validServerConfig(cert tls.Certificate) *config.ServerConfig {
	certPath, keyPath := writeTestCert(cert)
	return &config.ServerConfig{
		Listen: "127.0.0.1:0",
		Auth: config.AuthConfig{
			Type:     "password",
			Password: "pw",
		},
		TLS: config.TLSConfig{
			Cert: certPath,
			Key:  keyPath,
		},
		Control: config.ControlConfig{
			Token: "0123456789abcdef0123456789abcdef",
		},
		Web: config.WebConfig{Listen: ""},
		Log: config.LogConfig{Level: "info"},
	}
}

// writeTestCert writes cert and its private key as PEM files and returns their
// paths.
func writeTestCert(cert tls.Certificate) (certPath, keyPath string) {
	dir, err := os.MkdirTemp(testTempDir, "cert-*")
	if err != nil {
		panic(fmt.Sprintf("create certificate dir: %v", err))
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		panic(fmt.Sprintf("marshal private key: %v", err))
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		panic(fmt.Sprintf("write certificate: %v", err))
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		panic(fmt.Sprintf("write private key: %v", err))
	}
	return certPath, keyPath
}

// mustAddr parses a host:port string into a UDP address.
func mustAddr(s string) net.Addr {
	addr, err := net.ResolveUDPAddr("udp", s)
	if err != nil {
		panic(fmt.Sprintf("resolve address %q: %v", s, err))
	}
	return addr
}

// startTestServer starts a server with the default test configuration on a
// loopback ephemeral port and returns it with an idempotent stop function.
func startTestServer(t *testing.T) (*Server, func()) {
	t.Helper()

	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	return startServer(t, validServerConfig(cert))
}

// startServer starts a server with cfg and returns it with an idempotent stop
// function.
func startServer(t *testing.T, cfg *config.ServerConfig) (*Server, func()) {
	t.Helper()

	srv, err := New(cfg, slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Start(ctx) }()
	require.Eventually(t, func() bool {
		return srv.ListenAddr() != nil
	}, 2*time.Second, 10*time.Millisecond)

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			_ = srv.Close()
		})
	}
	t.Cleanup(stop)
	return srv, stop
}

func TestStandardHY2ClientCanProxyTCP(t *testing.T) {
	echoAddr, stopEcho := testutil.StartTCPEcho(t)
	defer stopEcho()

	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	cfg.Listen = "127.0.0.1:0"

	srv, err := New(cfg, slog.Default())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	defer srv.Close()

	cl, _, err := hyclient.NewClient(&hyclient.Config{
		ServerAddr: mustAddr(srv.ListenAddr().String()),
		Auth:       "pw",
		TLSConfig:  hyclient.TLSConfig{InsecureSkipVerify: true},
	})
	require.NoError(t, err)
	defer cl.Close()

	conn, err := cl.TCP(echoAddr)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf))
}

// newHY2Client dials srv with the official Hysteria client and registers
// cleanup. A nil factory uses the client default UDP socket.
func newHY2Client(t *testing.T, srv *Server, auth string, factory hyclient.ConnFactory) hyclient.Client {
	t.Helper()

	cl, _, err := hyclient.NewClient(&hyclient.Config{
		ServerAddr:  mustAddr(srv.ListenAddr().String()),
		Auth:        auth,
		TLSConfig:   hyclient.TLSConfig{InsecureSkipVerify: true},
		ConnFactory: factory,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}

// requireTCPEcho writes msg to conn and asserts that the same bytes come back.
func requireTCPEcho(t *testing.T, conn net.Conn, msg string) {
	t.Helper()

	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	_, err := conn.Write([]byte(msg))
	require.NoError(t, err)
	buf := make([]byte, len(msg))
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, msg, string(buf))
}

func TestOutboundProxiesNormalTCP(t *testing.T) {
	echoAddr, stopEcho := testutil.StartTCPEcho(t)
	defer stopEcho()

	o := &outbound{}
	conn, err := o.TCP(echoAddr)
	require.NoError(t, err)
	defer conn.Close()
	requireTCPEcho(t, conn, "ping")
}

func TestOutboundProxiesNormalUDP(t *testing.T) {
	echoAddr, stopEcho := testutil.StartUDPEcho(t)
	defer stopEcho()

	o := &outbound{}
	conn, err := o.UDP(echoAddr)
	require.NoError(t, err)
	defer conn.Close()

	uc, ok := conn.(*udpConn)
	require.True(t, ok)
	require.NoError(t, uc.SetReadDeadline(time.Now().Add(5*time.Second)))

	_, err = conn.WriteTo([]byte("ping"), echoAddr)
	require.NoError(t, err)
	buf := make([]byte, 16)
	n, from, err := conn.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf[:n]))
	require.Equal(t, echoAddr, from)
}

func TestOutboundRejectsVirtualAddresses(t *testing.T) {
	o := &outbound{}
	for _, addr := range []string{
		"_hy2tun_control_:0",
		"_hy2tun_data_abc123:0",
		"_hy2tun_udp_def456:0",
		"_hy2tun_unknown:0",
		"_hy2tun_data_:0",
	} {
		_, err := o.TCP(addr)
		require.Error(t, err, "TCP %s", addr)
		_, err = o.UDP(addr)
		require.Error(t, err, "UDP %s", addr)
	}
}

func TestServerAuthenticatesUserPass(t *testing.T) {
	echoAddr, stopEcho := testutil.StartTCPEcho(t)
	defer stopEcho()

	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	cfg.Auth = config.AuthConfig{
		Type:  "userpass",
		Users: map[string]string{"alice": "secret"},
	}
	srv, stop := startServer(t, cfg)
	defer stop()

	cl := newHY2Client(t, srv, "alice:secret", nil)
	conn, err := cl.TCP(echoAddr)
	require.NoError(t, err)
	defer conn.Close()
	requireTCPEcho(t, conn, "userpass")
}

// salamanderConnFactory wraps the client's UDP socket with Salamander
// obfuscation, matching the server-side wrapper.
type salamanderConnFactory struct {
	psk []byte
}

func (f *salamanderConnFactory) New(net.Addr) (net.PacketConn, error) {
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}
	wrapped, err := obfs.WrapPacketConnSalamander(conn, f.psk)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return wrapped, nil
}

func TestServerSupportsSalamanderObfs(t *testing.T) {
	echoAddr, stopEcho := testutil.StartTCPEcho(t)
	defer stopEcho()

	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	cfg.Obfs = config.ObfsConfig{
		Type:       "salamander",
		Salamander: config.SalamanderConfig{Password: "obfs-secret"},
	}
	srv, stop := startServer(t, cfg)
	defer stop()

	cl := newHY2Client(t, srv, "pw", &salamanderConnFactory{psk: []byte("obfs-secret")})
	conn, err := cl.TCP(echoAddr)
	require.NoError(t, err)
	defer conn.Close()
	requireTCPEcho(t, conn, "obfs")
}

func TestStandardHY2ClientCanProxyUDP(t *testing.T) {
	echoAddr, stopEcho := testutil.StartUDPEcho(t)
	defer stopEcho()

	srv, stop := startTestServer(t)
	defer stop()
	cl := newHY2Client(t, srv, "pw", nil)

	uc, err := cl.UDP()
	require.NoError(t, err)
	defer uc.Close()
	require.NoError(t, uc.Send([]byte("ping"), echoAddr))

	type udpResult struct {
		data []byte
		addr string
		err  error
	}
	ch := make(chan udpResult, 1)
	go func() {
		data, addr, err := uc.Receive()
		ch <- udpResult{data: data, addr: addr, err: err}
	}()
	select {
	case res := <-ch:
		require.NoError(t, res.err)
		require.Equal(t, "ping", string(res.data))
		require.NotEmpty(t, res.addr)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the UDP echo response")
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		_, err := New(nil, slog.Default())
		require.Error(t, err)
	})

	t.Run("missing TLS paths", func(t *testing.T) {
		cert, err := testutil.SelfSignedCert("localhost")
		require.NoError(t, err)
		cfg := validServerConfig(cert)
		cfg.TLS = config.TLSConfig{}
		_, err = New(cfg, slog.Default())
		require.ErrorContains(t, err, "tls.cert")
	})

	t.Run("missing certificate file", func(t *testing.T) {
		cert, err := testutil.SelfSignedCert("localhost")
		require.NoError(t, err)
		cfg := validServerConfig(cert)
		cfg.TLS.Cert = filepath.Join(testTempDir, "missing.pem")
		_, err = New(cfg, slog.Default())
		require.Error(t, err)
	})

	t.Run("invalid auth type", func(t *testing.T) {
		cert, err := testutil.SelfSignedCert("localhost")
		require.NoError(t, err)
		cfg := validServerConfig(cert)
		cfg.Auth = config.AuthConfig{Type: "bogus"}
		_, err = New(cfg, slog.Default())
		require.Error(t, err)
	})

	t.Run("unknown obfs type", func(t *testing.T) {
		cert, err := testutil.SelfSignedCert("localhost")
		require.NoError(t, err)
		cfg := validServerConfig(cert)
		cfg.Obfs.Type = "bogus"
		_, err = New(cfg, slog.Default())
		require.ErrorContains(t, err, "unsupported obfs.type")
	})

	t.Run("short salamander password", func(t *testing.T) {
		cert, err := testutil.SelfSignedCert("localhost")
		require.NoError(t, err)
		cfg := validServerConfig(cert)
		cfg.Obfs = config.ObfsConfig{
			Type:       "salamander",
			Salamander: config.SalamanderConfig{Password: "abc"},
		}
		_, err = New(cfg, slog.Default())
		require.Error(t, err)
	})
}

func TestServerCloseIsIdempotent(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	require.NoError(t, srv.Close())
	require.NoError(t, srv.Close())
}

func TestServerStartStopsOnContextCancel(t *testing.T) {
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	srv, err := New(validServerConfig(cert), slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()
	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
	require.NoError(t, srv.Close())
}

func TestServerWebAddr(t *testing.T) {
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	cfg.Web.Listen = "127.0.0.1:7500"
	cfg.ConfigPath = filepath.Join(t.TempDir(), "server.yaml")
	_, _, err = config.EnsureWebPassword(&cfg.Web)
	require.NoError(t, err)

	srv, err := New(cfg, slog.Default())
	require.NoError(t, err)
	defer srv.Close()

	require.Equal(t, "127.0.0.1:7500", srv.WebAddr())
}
