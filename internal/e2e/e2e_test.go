// Package e2e exercises a complete hy2tund + hy2tun deployment on loopback.
package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/client"
	"hy2tun/internal/config"
	"hy2tun/internal/server"
	"hy2tun/internal/testutil"
	"hy2tun/internal/view"
)

const (
	e2eControlToken = "0123456789abcdef0123456789abcdef"
	e2ePassword     = "hy2tun-e2e-password"
	e2eWebPassword  = "hy2tun-e2e-web-password"
)

// TestEndToEnd is the Task 13 acceptance test. It starts a real server and
// client with a self-signed certificate, verifies reverse TCP and UDP, checks
// that a standard HY2 client can still use the server as a proxy, and then
// drives the server web console to push and delete a mapping.
func TestEndToEnd(t *testing.T) {
	// 1. Start TCP and UDP echo servers. testutil registers their cleanup with
	// t.Cleanup, so they stay up until the tunnel is closed.
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)
	udpEchoAddr, _ := testutil.StartUDPEcho(t)

	// 2. Start hy2tund with self-signed TLS and a TCP + UDP mapping allowlist.
	dir := t.TempDir()
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	certPath, keyPath := writeCertificate(t, dir, cert)

	hash, err := bcryptHash(e2eWebPassword)
	require.NoError(t, err)

	serverConfigPath := filepath.Join(dir, "server.yaml")
	serverCfg := &config.ServerConfig{
		Listen: "127.0.0.1:0",
		Auth: config.AuthConfig{
			Type:     "password",
			Password: e2ePassword,
		},
		TLS: config.TLSConfig{Cert: certPath, Key: keyPath},
		Control: config.ControlConfig{
			Token: e2eControlToken,
		},
		Ports: config.PortsConfig{
			Allow:     []string{"20000-29999"},
			AutoRange: "20000-29999",
		},
		Web: config.WebConfig{
			Listen:       "127.0.0.1:0",
			Username:     "admin",
			PasswordHash: hash,
		},
		StateFile:  filepath.Join(dir, "state.json"),
		Log:        config.LogConfig{Level: "info"},
		ConfigPath: serverConfigPath,
	}
	require.NoError(t, config.SaveServer(serverConfigPath, serverCfg))
	loadedServerCfg, err := config.LoadServer(serverConfigPath)
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := server.New(loadedServerCfg, logger)
	require.NoError(t, err)

	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	go func() { _ = srv.Start(serverCtx) }()
	t.Cleanup(func() {
		stopServer()
		_ = srv.Close()
	})

	clientConfigPath := filepath.Join(dir, "client.yaml")
	clientCfg := &config.ClientConfig{
		Server: srv.ListenAddr().String(),
		Auth:   e2ePassword,
		TLS:    config.ClientTLSConfig{CA: certPath},
		Control: config.ControlConfig{
			Token: e2eControlToken,
		},
		Client: config.ClientIdentity{
			ID:   "e2e-client",
			Name: "e2e-client",
		},
		Mappings: []config.Mapping{
			{ID: "e2e-tcp", Type: "tcp", RemotePort: 0, LocalAddr: tcpEchoAddr, Note: "TCP echo", Enabled: true},
			{ID: "e2e-udp", Type: "udp", RemotePort: 0, LocalAddr: udpEchoAddr, Note: "UDP echo", Enabled: true},
		},
		Web: config.WebConfig{Listen: ""},
		Log: config.LogConfig{Level: "info"},
	}
	require.NoError(t, config.SaveClient(clientConfigPath, clientCfg))
	loadedClientCfg, err := config.LoadClient(clientConfigPath)
	require.NoError(t, err)

	cl, err := client.New(loadedClientCfg, logger)
	require.NoError(t, err)
	clientCtx, stopClient := context.WithCancel(context.Background())
	defer stopClient()
	go func() { _ = cl.Start(clientCtx) }()
	t.Cleanup(func() {
		stopClient()
		_ = cl.Close()
	})

	readyCtx, cancelReady := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelReady()
	require.NoError(t, cl.WaitReady(readyCtx))

	// 4. Wait for both mappings to be running with assigned public ports and
	// for the client's assigned-port write-back to settle. The revision must
	// be unchanged across two polls so a web push has a stable base revision.
	var (
		settled      view.ClientView
		lastRevision int64 = -1
		stablePolls  int
	)
	require.Eventually(t, func() bool {
		views := srv.Clients()
		if len(views) != 1 {
			lastRevision, stablePolls = -1, 0
			return false
		}
		candidate := views[0]
		if candidate.Revision == 0 || len(candidate.Mappings) != 2 {
			lastRevision, stablePolls = -1, 0
			return false
		}
		for _, mapping := range candidate.Mappings {
			if mapping.Status != "running" || mapping.AssignedPort == 0 {
				lastRevision, stablePolls = -1, 0
				return false
			}
		}
		if candidate.Revision != lastRevision {
			lastRevision, stablePolls = candidate.Revision, 1
			return false
		}
		stablePolls++
		if stablePolls < 2 {
			return false
		}
		settled = candidate
		return true
	}, 10*time.Second, 20*time.Millisecond, "both mappings must register, get ports, and settle")
	require.Equal(t, "e2e-client", settled.ID)

	tcpPort := mappingPort(t, settled, "e2e-tcp")
	udpPort := mappingPort(t, settled, "e2e-udp")
	require.NotZero(t, tcpPort)
	require.NotZero(t, udpPort)

	// 5. Verify TCP echo through the public port.
	tcpPublicAddr := fmt.Sprintf("127.0.0.1:%d", tcpPort)
	require.Eventually(t, func() bool {
		return tcpEcho(tcpPublicAddr, "tcp-through-reverse-mapping") == nil
	}, 5*time.Second, 50*time.Millisecond, "reverse TCP mapping must carry traffic")
	require.NoError(t, tcpEcho(tcpPublicAddr, "tcp-through-reverse-mapping"))

	// 6. Verify UDP echo through the public port. The first datagram may be
	// sent before the client's HY2 UDP session has started, so retry briefly.
	udpPublicAddr := fmt.Sprintf("127.0.0.1:%d", udpPort)
	require.Eventually(t, func() bool {
		return udpEcho(udpPublicAddr, "udp-through-reverse-mapping") == nil
	}, 5*time.Second, 50*time.Millisecond, "reverse UDP mapping must carry datagrams")
	require.NoError(t, udpEcho(udpPublicAddr, "udp-through-reverse-mapping"))

	// 7. Connect a standard HY2 core client and proxy TCP through the server.
	// It must work without knowing anything about the hy2tun control protocol.
	requireStandardHY2Proxy(t, srv, certPath, tcpEchoAddr)

	// 8. Log in to the server web, list clients, push a new mapping, and verify
	// the client config file changed.
	webAddr := waitForWeb(t, srv)
	webClient := &http.Client{Timeout: 10 * time.Second}
	cookie := loginWeb(t, webClient, webAddr)

	clients := listWebClients(t, webClient, webAddr, cookie)
	require.Len(t, clients, 1)
	require.Equal(t, settled.ID, clients[0].ID)
	require.Equal(t, settled.Revision, clients[0].Revision)

	baseMappings := mappingsFromView(clients[0].Mappings)
	require.Len(t, baseMappings, 2)
	pushedMapping := config.Mapping{
		ID:         "e2e-pushed",
		Type:       "tcp",
		RemotePort: 0,
		LocalAddr:  tcpEchoAddr,
		Note:       "pushed by e2e web test",
		Enabled:    true,
	}
	pushRevision := pushMappings(t, webClient, webAddr, cookie, clients[0].ID,
		clients[0].Revision, append(baseMappings, pushedMapping))
	require.Equal(t, settled.Revision+1, pushRevision)

	var pushed config.Mapping
	require.Eventually(t, func() bool {
		saved, err := config.LoadClient(clientConfigPath)
		if err != nil {
			return false
		}
		if saved.Client.Revision != pushRevision {
			return false
		}
		for _, mapping := range saved.Mappings {
			if mapping.ID == pushedMapping.ID {
				pushed = mapping
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "pushed mapping must be persisted by the client")
	require.NotZero(t, pushed.RemotePort, "the client must persist the server-assigned public port")
	require.Equal(t, "pushed by e2e web test", pushed.Note)

	// The pushed mapping is live before we delete it.
	pushedAddr := fmt.Sprintf("127.0.0.1:%d", pushed.RemotePort)
	require.Eventually(t, func() bool {
		return tcpEcho(pushedAddr, "pushed-mapping-is-live") == nil
	}, 5*time.Second, 50*time.Millisecond, "pushed mapping must be bound and serving")
	require.NoError(t, tcpEcho(pushedAddr, "pushed-mapping-is-live"))

	// 9. Delete the mapping through the same authoritative push path and verify
	// the public port closes.
	deleteRevision := pushMappings(t, webClient, webAddr, cookie, clients[0].ID,
		pushRevision, baseMappings)
	require.Equal(t, pushRevision+1, deleteRevision)

	require.Eventually(t, func() bool {
		saved, err := config.LoadClient(clientConfigPath)
		if err != nil {
			return false
		}
		if saved.Client.Revision != deleteRevision || len(saved.Mappings) != 2 {
			return false
		}
		for _, mapping := range saved.Mappings {
			if mapping.ID == pushedMapping.ID {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond, "deleted mapping must be removed from the client config")

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", pushedAddr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return false
		}
		// A closed listener must refuse the connection. A timeout means the
		// packet was dropped (for example by a firewall) and does not prove
		// that the public port was released.
		return errors.Is(err, syscall.ECONNREFUSED)
	}, 5*time.Second, 50*time.Millisecond, "deleting the mapping must close its public TCP port")

	// The original mappings must keep working after the delete.
	require.NoError(t, tcpEcho(tcpPublicAddr, "survives-delete"))
	require.NoError(t, udpEcho(udpPublicAddr, "survives-delete"))
}

// TestExamplesValidate keeps the shipped example configurations loadable.
func TestExamplesValidate(t *testing.T) {
	serverCfg, err := config.LoadServer(filepath.Join("..", "..", "examples", "server.yaml"))
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:7500", serverCfg.Web.Listen)
	require.NotEmpty(t, serverCfg.Control.Token)
	require.NotEmpty(t, serverCfg.TLS.Cert)

	clientCfg, err := config.LoadClient(filepath.Join("..", "..", "examples", "client.yaml"))
	require.NoError(t, err)
	require.Equal(t, "0.0.0.0:7501", clientCfg.Web.Listen)
	require.Equal(t, "127.0.0.1:1080", clientCfg.Proxy.SOCKS5)
	require.Equal(t, "127.0.0.1:8080", clientCfg.Proxy.HTTP)
	require.NotEmpty(t, clientCfg.Mappings)
}

// requireStandardHY2Proxy connects the official Hysteria core client and
// verifies that its TCP proxy path reaches the echo server.
func requireStandardHY2Proxy(t *testing.T, srv *server.Server, certPath, echoAddr string) {
	t.Helper()

	pool := x509.NewCertPool()
	certPEM, err := os.ReadFile(certPath)
	require.NoError(t, err)
	require.True(t, pool.AppendCertsFromPEM(certPEM))

	hy, _, err := hyclient.NewClient(&hyclient.Config{
		ServerAddr: srv.ListenAddr(),
		Auth:       e2ePassword,
		TLSConfig: hyclient.TLSConfig{
			ServerName: "localhost",
			RootCAs:    pool,
		},
	})
	require.NoError(t, err)
	defer func() { _ = hy.Close() }()

	conn, err := hy.TCP(echoAddr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	requireTCPEcho(t, conn, "standard-hy2-proxy")
}

// waitForWeb waits until the server web listener has a concrete address and
// answers the login page.
func waitForWeb(t *testing.T, srv *server.Server) string {
	t.Helper()

	client := &http.Client{Timeout: 2 * time.Second}
	var webAddr string
	require.Eventually(t, func() bool {
		webAddr = srv.WebAddr()
		if webAddr == "" || webAddr == "127.0.0.1:0" {
			return false
		}
		resp, err := client.Get("http://" + webAddr + "/login")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond, "server web console must become reachable")
	return webAddr
}

// loginWeb authenticates with the form API and returns the session cookie.
func loginWeb(t *testing.T, client *http.Client, webAddr string) *http.Cookie {
	t.Helper()

	resp, err := client.PostForm("http://"+webAddr+"/api/login", url.Values{
		"username": {"admin"},
		"password": {e2eWebPassword},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	cookies := resp.Cookies()
	require.NotEmpty(t, cookies)
	return cookies[0]
}

// listWebClients reads the authenticated client list.
func listWebClients(t *testing.T, client *http.Client, webAddr string, cookie *http.Cookie) []view.ClientView {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, "http://"+webAddr+"/api/clients", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var clients []view.ClientView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&clients))
	return clients
}

// pushMappings sends an authoritative mapping list through the web API and
// returns the new revision.
func pushMappings(t *testing.T, client *http.Client, webAddr string, cookie *http.Cookie, clientID string, baseRevision int64, mappings []config.Mapping) int64 {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"base_revision": baseRevision,
		"mappings":      mappings,
	})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPut,
		"http://"+webAddr+"/api/clients/"+url.PathEscape(clientID)+"/mappings",
		bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "hy2tun")
	req.Header.Set("Origin", "http://"+webAddr)
	req.AddCookie(cookie)

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(resp.Body)
		t.Fatalf("push mappings: status %d: %s", resp.StatusCode, message)
	}

	var result struct {
		Revision int64 `json:"revision"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.NotZero(t, result.Revision)
	return result.Revision
}

// mappingsFromView copies the mapping definitions out of read models.
func mappingsFromView(views []view.MappingView) []config.Mapping {
	mappings := make([]config.Mapping, 0, len(views))
	for _, mapping := range views {
		mappings = append(mappings, mapping.Mapping)
	}
	return mappings
}

// mappingPort returns the assigned public port for a mapping ID.
func mappingPort(t *testing.T, clientView view.ClientView, id string) int {
	t.Helper()

	for _, mapping := range clientView.Mappings {
		if mapping.ID == id {
			return mapping.AssignedPort
		}
	}
	t.Fatalf("mapping %q not found in client view", id)
	return 0
}

// tcpEcho writes msg through addr and expects the same bytes back.
func tcpEcho(addr, msg string) error {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	return echoOnConn(conn, msg)
}

// udpEcho sends one datagram through addr and expects the same payload back.
func udpEcho(addr, msg string) error {
	conn, err := net.DialTimeout("udp", addr, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte(msg)); err != nil {
		return err
	}
	buf := make([]byte, len(msg))
	n, err := io.ReadFull(conn, buf)
	if err != nil {
		return err
	}
	if string(buf[:n]) != msg {
		return fmt.Errorf("udp echo: got %q, want %q", buf[:n], msg)
	}
	return nil
}

// requireTCPEcho writes msg to conn and asserts that the same bytes come back.
func requireTCPEcho(t *testing.T, conn net.Conn, msg string) {
	t.Helper()

	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, echoOnConn(conn, msg))
}

// echoOnConn performs one write/read round trip on an established connection.
func echoOnConn(conn net.Conn, msg string) error {
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	if _, err := conn.Write([]byte(msg)); err != nil {
		return err
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if string(buf) != msg {
		return fmt.Errorf("tcp echo: got %q, want %q", buf, msg)
	}
	return nil
}

// writeCertificate writes a TLS certificate and its PKCS#8 private key as PEM
// files for the server configuration.
func writeCertificate(t *testing.T, dir string, cert tls.Certificate) (certPath, keyPath string) {
	t.Helper()

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0600))
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0600))
	return certPath, keyPath
}

// bcryptHash produces the bcrypt hash the server web console expects.
func bcryptHash(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}
