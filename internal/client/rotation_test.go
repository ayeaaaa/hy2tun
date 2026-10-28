package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/server"
	"hy2tun/internal/testutil"
)

// testPair is a running server and client used by client-side integration
// tests. It mirrors the server package helper so client tests can exercise the
// real control handshake without depending on server test files.
type testPair struct {
	Server           *server.Server
	Client           *Client
	ClientConfigPath string
	Stop             func()
}

// startTestPair starts a server and a client configured with mappings, waits
// for the client's control handshake to complete, and returns both. The caller
// must call Stop.
func startTestPair(t *testing.T, mappings []config.Mapping) *testPair {
	t.Helper()

	srv, stopServer := startTestServerForClient(t)

	cfg := testClientConfigForServer(srv.ListenAddr().String(), mappings)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "client.yaml")
	require.NoError(t, config.SaveClient(configPath, cfg))

	cl, err := New(cfg, slog.Default())
	require.NoError(t, err)
	go func() { _ = cl.Start(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, cl.WaitReady(ctx))

	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cl.Close()
			stopServer()
		})
	}
	t.Cleanup(stop)

	return &testPair{
		Server:           srv,
		Client:           cl,
		ClientConfigPath: configPath,
		Stop:             stop,
	}
}

// startTestServerForClient starts a password-authenticated server on a
// loopback ephemeral port with a temporary self-signed certificate.
func startTestServerForClient(t *testing.T) (*server.Server, func()) {
	t.Helper()

	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	certPath, keyPath := writeTestCertFiles(t, cert)

	cfg := &config.ServerConfig{
		Listen: "127.0.0.1:0",
		Auth: config.AuthConfig{
			Type:     "password",
			Password: "pw",
		},
		TLS: config.TLSConfig{
			Cert: certPath,
			Key:  keyPath,
		},
		Control: config.ControlConfig{Token: "0123456789abcdef0123456789abcdef"},
		Web:     config.WebConfig{Listen: ""},
		Log:     config.LogConfig{Level: "info"},
	}
	srv, err := server.New(cfg, slog.Default())
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

// writeTestCertFiles writes a certificate and its PKCS#8 private key as PEM
// files and returns their paths.
func writeTestCertFiles(t *testing.T, cert tls.Certificate) (certPath, keyPath string) {
	t.Helper()

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0600))
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0600))
	return certPath, keyPath
}

// testClientConfigForServer builds a valid client configuration for the given
// server and mappings. Every call gets a unique client ID.
func testClientConfigForServer(serverAddr string, mappings []config.Mapping) *config.ClientConfig {
	id, err := newClientID()
	if err != nil {
		panic(fmt.Sprintf("generate client ID: %v", err))
	}
	return &config.ClientConfig{
		Server: serverAddr,
		Auth:   "pw",
		TLS:    config.ClientTLSConfig{Insecure: true},
		Control: config.ControlConfig{
			Token: "0123456789abcdef0123456789abcdef",
		},
		Client: config.ClientIdentity{
			ID:   id,
			Name: "test-client",
		},
		Mappings: append([]config.Mapping(nil), mappings...),
		Credentials: config.CredentialsConfig{
			Generation: 1,
		},
		Web: config.WebConfig{Listen: ""},
		Log: config.LogConfig{Level: "info"},
	}
}

// waitFor polls cond every 20ms until it returns true or d elapses.
func waitFor(t *testing.T, d time.Duration, cond func() bool) error {
	t.Helper()

	deadline := time.Now().Add(d)
	for {
		if cond() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("condition not met within %s", d)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestClientRotateLocalCredentialsWritesConfigAndReconnects(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()
	require.NoError(t, waitFor(t, 5*time.Second, func() bool { return len(pair.Server.Clients()) == 1 }))

	gen := pair.Server.CredentialsSnapshot().Generation + 1
	// Manual server rotation first, so the server accepts the new values.
	_, _, err := pair.Server.RotateCredentials("new-auth", "new-control-token", "manual")
	require.NoError(t, err)
	require.NoError(t, pair.Client.RotateLocalCredentials(gen, "new-auth", "new-control-token"))

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && v[0].CredentialGeneration == gen && v[0].CredentialSynced
	}))
}

func TestClientRotateLocalCredentialsPersistsAndRejectsStale(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 1)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	require.Error(t, cl.RotateLocalCredentials(1, "new-auth", "new-token-0123456789abcdef"))
	require.Equal(t, int64(1), cl.Status().CredentialGeneration)

	require.NoError(t, cl.RotateLocalCredentials(2, "new-auth", "new-token-0123456789abcdef"))

	saved, err := config.LoadClient(cfgPath)
	require.NoError(t, err)
	require.Equal(t, int64(2), saved.Credentials.Generation)
	require.Equal(t, "new-auth", saved.Auth)
	require.Equal(t, "new-token-0123456789abcdef", saved.Control.Token)

	snapshot := cl.CredentialsSnapshot()
	require.Equal(t, int64(2), snapshot.Generation)
	require.Equal(t, "new-auth", snapshot.Auth)
	require.Equal(t, "new-token-0123456789abcdef", snapshot.ControlToken)

	status := cl.Status()
	require.Equal(t, int64(2), status.CredentialGeneration)
	require.False(t, status.CredentialSynced)
}

// TestClientRotateDuringConnectAbandonsStaleConnection covers the window where
// runOnce has already built its HY2 data-plane config with the old auth but has
// not yet installed the connection. A rotation in that window must not be lost:
// the old-auth connection has to be abandoned and the next attempt has to use
// the new credentials.
func TestClientRotateDuringConnectAbandonsStaleConnection(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 1)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	fake := newGatedSetupHY2Client()
	started := make(chan struct{})
	release := make(chan struct{})
	stopConnect := errors.New("stop test connect")

	var (
		mu       sync.Mutex
		auths    []string
		attempts int
	)
	cl.newHY2 = func(cfg *hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error) {
		mu.Lock()
		attempts++
		attempt := attempts
		auths = append(auths, cfg.Auth)
		mu.Unlock()

		if attempt == 1 {
			close(started)
			<-release
			return fake, nil, nil
		}
		return nil, nil, stopConnect
	}

	done := make(chan error, 1)
	go func() {
		_, runErr := cl.runOnce(context.Background())
		done <- runErr
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not start")
	}

	require.NoError(t, cl.RotateLocalCredentials(2, "new-auth", "new-token-0123456789abcdef"))
	close(release)

	select {
	case runErr := <-done:
		require.NoError(t, runErr, "the stale connection setup must be abandoned cleanly")
	case <-time.After(2 * time.Second):
		t.Fatal("runOnce did not abandon the stale connection")
	}

	require.True(t, fake.isClosed(), "the stale HY2 client must be closed")
	tracking := fake.trackingConn()
	require.NotNil(t, tracking)
	require.False(t, tracking.wrote.Load(), "the stale connection must not reach the Hello handshake")

	// The retry must build its HY2 config from the rotated auth.
	_, err = cl.runOnce(context.Background())
	require.ErrorIs(t, err, stopConnect)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, auths, 2)
	require.Equal(t, "new-auth", auths[1])
}

func TestClientNameUpdateWritesConfigAndNotifiesServer(t *testing.T) {
	pair := startTestPair(t, nil)
	defer pair.Stop()
	require.NoError(t, waitFor(t, 5*time.Second, func() bool { return len(pair.Server.Clients()) == 1 }))

	before := pair.Client.Revision()
	require.NoError(t, pair.Client.UpdateName("renamed"))
	require.Equal(t, before+1, pair.Client.Revision())

	saved, err := config.LoadClient(pair.ClientConfigPath)
	require.NoError(t, err)
	require.Equal(t, "renamed", saved.Client.Name)
	require.Equal(t, before+1, saved.Client.Revision)
	require.Equal(t, "renamed", pair.Client.Status().Name)

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		clients := pair.Server.Clients()
		return len(clients) == 1 && clients[0].Name == "renamed"
	}))
}

func TestClientNameUpdateRejectsEmptyName(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 1)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	require.Error(t, cl.UpdateName("  "))
	require.Equal(t, int64(1), cl.Revision())
}

// TestClientNameUpdateDuringHandshakeIsAnnounced simulates a rename while the
// HelloAck is in flight. The rename cannot be sent before the handshake is
// marked connected, so applyHelloAck's follow-up update must carry the current
// name instead of an empty one.
func TestClientNameUpdateDuringHandshakeIsAnnounced(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 3)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	cl.mu.Lock()
	cl.controlConn = local
	cl.mu.Unlock()

	// handshake snapshots the pre-rename configuration.
	snapshot := cl.snapshotConfig()
	require.NoError(t, cl.UpdateName("new-name"))
	require.Equal(t, int64(4), cl.Revision())

	updates := make(chan *control.Update, 1)
	go func() {
		msg, readErr := control.ReadMessage(remote)
		if readErr != nil {
			updates <- nil
			return
		}
		update, _ := msg.(*control.Update)
		updates <- update
	}()

	cl.markConnected()
	require.NoError(t, cl.applyHelloAck(snapshot, &control.HelloAck{
		Type:                 control.TypeHelloAck,
		OK:                   true,
		ServerVersion:        "test",
		CredentialGeneration: 1,
		CredentialSynced:     true,
	}))

	var update *control.Update
	select {
	case update = <-updates:
	case <-time.After(2 * time.Second):
		t.Fatal("applyHelloAck did not announce the concurrent rename")
	}
	require.NotNil(t, update)
	require.Equal(t, int64(4), update.Revision)
	require.Equal(t, "new-name", update.Name)
}

// TestClientNameUpdateDuringHandshakeReachesServer uses a real server and
// pauses the client's HelloAck read so the rename lands in the exact handshake
// window. The follow-up update must still reach the server with the new name.
func TestClientNameUpdateDuringHandshakeReachesServer(t *testing.T) {
	srv, stopServer := startTestServerForClient(t)
	defer stopServer()

	cfg := testClientConfigForServer(srv.ListenAddr().String(), nil)
	configPath := filepath.Join(t.TempDir(), "client.yaml")
	require.NoError(t, config.SaveClient(configPath, cfg))

	cl, err := New(cfg, slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	gate := newHandshakeGate()
	realNew := hyclient.NewClient
	cl.newHY2 = func(cfg *hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error) {
		hy, info, err := realNew(cfg)
		if err != nil {
			return nil, nil, err
		}
		return &gatedReadHY2Client{Client: hy, gate: gate}, info, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cl.Start(ctx) }()

	select {
	case <-gate.readStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("client did not reach the HelloAck read")
	}

	require.NoError(t, cl.UpdateName("new-name"))
	close(gate.release)

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		clients := srv.Clients()
		return len(clients) == 1 && clients[0].Name == "new-name"
	}))

	_ = cl.Close()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("client Start did not return after Close")
	}
}

func TestClientStatusTracksHelloAckCredentialState(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 1)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	require.NoError(t, cl.applyHelloAck(cl.snapshotConfig(), &control.HelloAck{
		Type:                 control.TypeHelloAck,
		OK:                   true,
		ServerVersion:        "test",
		CredentialGeneration: 3,
		CredentialSynced:     true,
	}))

	status := cl.Status()
	require.Equal(t, int64(1), status.CredentialGeneration)
	require.Equal(t, int64(3), status.ServerCredentialGeneration)
	require.True(t, status.CredentialSynced)
	require.Equal(t, int64(3), cl.ServerCredentialGeneration())
	require.True(t, cl.CredentialSynced())
}

func TestClientStatusTreatsLegacyHelloAckAsUnknown(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 1)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	// A server that predates credential rotation omits both fields. That is
	// "unknown", not generation 1, even if the ack carries a stale true.
	require.NoError(t, cl.applyHelloAck(cl.snapshotConfig(), &control.HelloAck{
		Type:             control.TypeHelloAck,
		OK:               true,
		ServerVersion:    "legacy",
		CredentialSynced: true,
	}))

	status := cl.Status()
	require.Equal(t, int64(0), status.ServerCredentialGeneration)
	require.False(t, status.CredentialSynced)
	require.Equal(t, int64(0), cl.ServerCredentialGeneration())
	require.False(t, cl.CredentialSynced())
}

func TestClientUpdateWebConfigWritesConfigWithoutReconnect(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 3)
	oldHash, err := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	require.NoError(t, err)
	cfg := loadClientConfig(t, cfgPath)
	cfg.Web = config.WebConfig{Username: "admin", PasswordHash: string(oldHash)}
	require.NoError(t, config.SaveClient(cfgPath, cfg))

	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	newHash, err := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
	require.NoError(t, err)
	before := cl.Revision()
	require.NoError(t, cl.UpdateWebConfig("root", string(newHash)))
	require.Equal(t, before, cl.Revision())

	saved, err := config.LoadClient(cfgPath)
	require.NoError(t, err)
	require.Equal(t, "root", saved.Web.Username)
	require.Equal(t, string(newHash), saved.Web.PasswordHash)

	require.Error(t, cl.UpdateWebConfig("", string(newHash)))
	require.Error(t, cl.UpdateWebConfig("root", "not-a-bcrypt-hash"))
}

// writeTrackingConn records whether the control handshake attempted a write.
type writeTrackingConn struct {
	net.Conn
	wrote atomic.Bool
}

func (c *writeTrackingConn) Write(p []byte) (int, error) {
	c.wrote.Store(true)
	return c.Conn.Write(p)
}

// gatedSetupHY2Client is a HY2 client whose control stream is a net.Pipe with
// its far end already closed, so any handshake write fails immediately without
// blocking the test.
type gatedSetupHY2Client struct {
	closed    chan struct{}
	closeOnce sync.Once

	mu   sync.Mutex
	conn *writeTrackingConn
}

func newGatedSetupHY2Client() *gatedSetupHY2Client {
	return &gatedSetupHY2Client{closed: make(chan struct{})}
}

func (f *gatedSetupHY2Client) TCP(string) (net.Conn, error) {
	local, remote := net.Pipe()
	_ = remote.Close()

	conn := &writeTrackingConn{Conn: local}
	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()
	return conn, nil
}

func (f *gatedSetupHY2Client) UDP() (hyclient.HyUDPConn, error) {
	return nil, fmt.Errorf("UDP is not used by this test")
}

func (f *gatedSetupHY2Client) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *gatedSetupHY2Client) isClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}

func (f *gatedSetupHY2Client) trackingConn() *writeTrackingConn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conn
}

// handshakeGate blocks the first control-stream read until release is closed.
type handshakeGate struct {
	readStarted chan struct{}
	release     chan struct{}
	once        sync.Once
}

func newHandshakeGate() *handshakeGate {
	return &handshakeGate{
		readStarted: make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (g *handshakeGate) waitForRead() {
	g.once.Do(func() {
		close(g.readStarted)
		<-g.release
	})
}

// gatedReadHY2Client wraps a real HY2 client and gates the control stream's
// first read so a test can act between Hello and HelloAck.
type gatedReadHY2Client struct {
	hyclient.Client
	gate *handshakeGate
}

func (c *gatedReadHY2Client) TCP(addr string) (net.Conn, error) {
	conn, err := c.Client.TCP(addr)
	if err != nil {
		return nil, err
	}
	if addr != controlAddress {
		return conn, nil
	}
	return &gatedReadConn{Conn: conn, gate: c.gate}, nil
}

type gatedReadConn struct {
	net.Conn
	gate *handshakeGate
}

func (c *gatedReadConn) Read(p []byte) (int, error) {
	c.gate.waitForRead()
	return c.Conn.Read(p)
}
