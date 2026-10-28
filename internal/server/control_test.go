package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/client"
	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/state"
	"hy2tun/internal/testutil"
)

// testPair is a running server and client used by the control-plane tests.
type testPair struct {
	Server           *Server
	Client           *client.Client
	ClientConfigPath string
	Stop             func()
}

// startTestPair starts a server and a client configured with mappings, waits
// for the client's control handshake to complete, and returns both. The caller
// must call Stop.
func startTestPair(t *testing.T, mappings []config.Mapping) *testPair {
	t.Helper()

	srv, stopServer := startTestServer(t)

	cfg := testClientConfig(srv.ListenAddr().String(), mappings)
	dir, err := os.MkdirTemp("", "hy2tun-client-test-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	configPath := filepath.Join(dir, "client.yaml")
	require.NoError(t, config.SaveClient(configPath, cfg))

	cl, err := client.New(cfg, slog.Default())
	require.NoError(t, err)
	go func() { _ = cl.Start(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, cl.WaitReady(ctx))

	// A client that receives server-assigned ports writes them back and sends
	// an update before its revision is stable. Wait for that write-back so
	// tests do not race a push against the client's own update.
	needsPortWriteback := false
	for _, mapping := range mappings {
		if mapping.Enabled && mapping.RemotePort == 0 {
			needsPortWriteback = true
			break
		}
	}
	if needsPortWriteback {
		require.NoError(t, waitFor(t, 5*time.Second, func() bool {
			views := srv.Clients()
			if len(views) != 1 {
				return false
			}
			if views[0].Revision > 0 {
				return true
			}
			for _, mapping := range views[0].Mappings {
				if mapping.Status == "failed" {
					return true
				}
			}
			return false
		}))
	}

	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cl.Close()
			stopServer()
			_ = os.RemoveAll(dir)
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

// testClientConfig builds a valid client configuration for the given server
// and mappings. Every call gets a unique client ID.
func testClientConfig(serverAddr string, mappings []config.Mapping) *config.ClientConfig {
	var idBytes [8]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
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
			ID:   "test-" + hex.EncodeToString(idBytes[:]),
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

func TestClientRegistersTCPAndUDPMappings(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		views := pair.Server.Clients()
		return len(views) == 1 && len(views[0].Mappings) == 2
	}))

	views := pair.Server.Clients()
	require.Equal(t, "running", views[0].Mappings[0].Status)
	require.NotZero(t, views[0].Mappings[0].AssignedPort)
	require.Equal(t, "running", views[0].Mappings[1].Status)
	require.NotZero(t, views[0].Mappings[1].AssignedPort)

	clientViews := pair.Client.Mappings()
	require.Len(t, clientViews, 2)
	require.Equal(t, "running", clientViews[0].Status)
	require.Equal(t, views[0].Mappings[0].AssignedPort, clientViews[0].AssignedPort)
	require.Equal(t, "running", clientViews[1].Status)
	require.Equal(t, views[0].Mappings[1].AssignedPort, clientViews[1].AssignedPort)
}

func TestHelloCarriesCredentialGeneration(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && v[0].CredentialGeneration == 1 && v[0].CredentialSynced
	}))
}

func TestHelloAcceptsPreviousCredentials(t *testing.T) {
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	cfg.Auth.Password = "new-pw"
	cfg.Control.Token = strings.Repeat("a", 32)
	cfg.Credentials = config.CredentialsConfig{
		Generation:           2,
		PreviousGeneration:   1,
		PreviousAuth:         "old-pw",
		PreviousControlToken: strings.Repeat("b", 32),
	}
	srv, stop := startServer(t, cfg)
	defer stop()

	// The previous HY2 password must still authenticate a new connection.
	hy := newHY2Client(t, srv, "old-pw", nil)
	conn, err := hy.TCP("_hy2tun_control_:0")
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, control.WriteMessage(conn, &control.Hello{
		Type:     control.TypeHello,
		Protocol: control.ProtocolVersion,
		Token:    strings.Repeat("b", 32),
		ClientID: "legacy-client",
		Name:     "legacy",
		Version:  "test",
		Revision: 0,
	}))
	msg, err := control.ReadMessage(conn)
	require.NoError(t, err)
	ack, ok := msg.(*control.HelloAck)
	require.True(t, ok)
	require.True(t, ack.OK)
	require.Equal(t, int64(2), ack.CredentialGeneration)
	require.False(t, ack.CredentialSynced)

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := srv.Clients()
		return len(v) == 1 &&
			v[0].CredentialGeneration == 1 &&
			!v[0].CredentialSynced &&
			!v[0].CredentialCapable
	}))
}

func TestHelloCurrentGenerationAndCapability(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	hy := newHY2Client(t, srv, "pw", nil)
	conn, err := hy.TCP("_hy2tun_control_:0")
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, control.WriteMessage(conn, &control.Hello{
		Type:                 control.TypeHello,
		Protocol:             control.ProtocolVersion,
		Token:                "0123456789abcdef0123456789abcdef",
		ClientID:             "capable-client",
		Name:                 "capable",
		Version:              "test",
		Revision:             0,
		CredentialGeneration: 1,
		Capabilities:         []string{control.CapabilityCredentialUpdate},
	}))
	msg, err := control.ReadMessage(conn)
	require.NoError(t, err)
	ack, ok := msg.(*control.HelloAck)
	require.True(t, ok)
	require.True(t, ack.OK)
	require.Equal(t, int64(1), ack.CredentialGeneration)
	require.True(t, ack.CredentialSynced)

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := srv.Clients()
		return len(v) == 1 &&
			v[0].CredentialGeneration == 1 &&
			v[0].CredentialSynced &&
			v[0].CredentialCapable
	}))
}

func TestCredentialAckIsForwardedToSession(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	hy := newHY2Client(t, srv, "pw", nil)
	conn, err := hy.TCP("_hy2tun_control_:0")
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, control.WriteMessage(conn, &control.Hello{
		Type:                 control.TypeHello,
		Protocol:             control.ProtocolVersion,
		Token:                "0123456789abcdef0123456789abcdef",
		ClientID:             "ack-client",
		Name:                 "ack",
		Version:              "test",
		Revision:             0,
		CredentialGeneration: 1,
		Capabilities:         []string{control.CapabilityCredentialUpdate},
	}))
	msg, err := control.ReadMessage(conn)
	require.NoError(t, err)
	_, ok := msg.(*control.HelloAck)
	require.True(t, ok)

	srv.mappings.mu.Lock()
	sess := srv.mappings.sessions["ack-client"]
	srv.mappings.mu.Unlock()
	require.NotNil(t, sess)

	require.NoError(t, control.WriteMessage(conn, &control.CredentialAck{
		Type:       control.TypeCredentialAck,
		Generation: 2,
		OK:         true,
	}))
	select {
	case ack := <-sess.credentialAck:
		require.NotNil(t, ack)
		require.Equal(t, int64(2), ack.Generation)
		require.True(t, ack.OK)
		require.Empty(t, ack.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("credential ack was not forwarded to the session")
	}

	// A failed ack must reach the waiter with OK=false so rotation can report
	// the failure instead of treating it as synced.
	require.NoError(t, control.WriteMessage(conn, &control.CredentialAck{
		Type:       control.TypeCredentialAck,
		Generation: 2,
		OK:         false,
		Error:      "write_failed",
	}))
	select {
	case ack := <-sess.credentialAck:
		require.NotNil(t, ack)
		require.Equal(t, int64(2), ack.Generation)
		require.False(t, ack.OK)
		require.Equal(t, "write_failed", ack.Error)
	case <-time.After(2 * time.Second):
		t.Fatal("failed credential ack was not forwarded to the session")
	}
}

func TestUpdateNameOnlyKeepsMappingsAndPersists(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	hy := newHY2Client(t, srv, "pw", nil)
	conn, err := hy.TCP("_hy2tun_control_:0")
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, control.WriteMessage(conn, &control.Hello{
		Type:     control.TypeHello,
		Protocol: control.ProtocolVersion,
		Token:    "0123456789abcdef0123456789abcdef",
		ClientID: "rename-client",
		Name:     "before",
		Version:  "test",
		Revision: 0,
		Mappings: []config.Mapping{
			{ID: "keep", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
		},
	}))
	msg, err := control.ReadMessage(conn)
	require.NoError(t, err)
	helloAck, ok := msg.(*control.HelloAck)
	require.True(t, ok)
	require.Len(t, helloAck.Results, 1)
	require.True(t, helloAck.Results[0].OK)
	assignedPort := helloAck.Results[0].AssignedPort
	require.NotZero(t, assignedPort)

	// A nil Mappings field means "no mapping change", so a name-only update
	// must leave the live listener running.
	require.NoError(t, control.WriteMessage(conn, &control.Update{
		Type:     control.TypeUpdate,
		Revision: 1,
		Name:     "after",
	}))
	msg, err = control.ReadMessage(conn)
	require.NoError(t, err)
	ack, ok := msg.(*control.UpdateAck)
	require.True(t, ok)
	require.Equal(t, int64(1), ack.Revision)
	require.Len(t, ack.Results, 1)
	require.Equal(t, "keep", ack.Results[0].ID)
	require.True(t, ack.Results[0].OK)
	require.Equal(t, assignedPort, ack.Results[0].AssignedPort)

	client, ok := srv.Client("rename-client")
	require.True(t, ok)
	require.Equal(t, "after", client.Name)
	require.Len(t, client.Mappings, 1)
	require.Equal(t, "keep", client.Mappings[0].ID)
	require.Equal(t, "running", client.Mappings[0].Status)
	require.Equal(t, assignedPort, client.Mappings[0].AssignedPort)

	snapshot := srv.store.Snapshot()
	require.Equal(t, "after", snapshot.Clients["rename-client"].Name)
	require.Len(t, snapshot.Clients["rename-client"].Mappings, 1)
}

func TestUpdateNameWithExplicitEmptyMappingsClearsMappings(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	hy := newHY2Client(t, srv, "pw", nil)
	conn, err := hy.TCP("_hy2tun_control_:0")
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, control.WriteMessage(conn, &control.Hello{
		Type:     control.TypeHello,
		Protocol: control.ProtocolVersion,
		Token:    "0123456789abcdef0123456789abcdef",
		ClientID: "clear-client",
		Name:     "before",
		Version:  "test",
		Revision: 0,
		Mappings: []config.Mapping{
			{ID: "remove", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
		},
	}))
	msg, err := control.ReadMessage(conn)
	require.NoError(t, err)
	helloAck, ok := msg.(*control.HelloAck)
	require.True(t, ok)
	require.Len(t, helloAck.Results, 1)
	require.True(t, helloAck.Results[0].OK)

	// An explicit empty slice is authoritative and clears every mapping.
	require.NoError(t, control.WriteMessage(conn, &control.Update{
		Type:     control.TypeUpdate,
		Revision: 1,
		Name:     "after",
		Mappings: []config.Mapping{},
	}))
	msg, err = control.ReadMessage(conn)
	require.NoError(t, err)
	ack, ok := msg.(*control.UpdateAck)
	require.True(t, ok)
	require.Equal(t, int64(1), ack.Revision)
	require.Empty(t, ack.Results)

	client, ok := srv.Client("clear-client")
	require.True(t, ok)
	require.Equal(t, "after", client.Name)
	require.Empty(t, client.Mappings)
}

func TestUpdateNilMappingsWithoutNameClearsLastMapping(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	hy := newHY2Client(t, srv, "pw", nil)
	conn, err := hy.TCP("_hy2tun_control_:0")
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, control.WriteMessage(conn, &control.Hello{
		Type:     control.TypeHello,
		Protocol: control.ProtocolVersion,
		Token:    "0123456789abcdef0123456789abcdef",
		ClientID: "legacy-clear-client",
		Name:     "before",
		Version:  "test",
		Revision: 0,
		Mappings: []config.Mapping{
			{ID: "last", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
		},
	}))
	msg, err := control.ReadMessage(conn)
	require.NoError(t, err)
	helloAck, ok := msg.(*control.HelloAck)
	require.True(t, ok)
	require.Len(t, helloAck.Results, 1)
	require.True(t, helloAck.Results[0].OK)
	assignedPort := helloAck.Results[0].AssignedPort
	require.NotZero(t, assignedPort)

	// Old clients marshal an empty mapping slice as mappings:null. With no
	// name this is an explicit clear, not a metadata-only update.
	require.NoError(t, control.WriteMessage(conn, &control.Update{
		Type:     control.TypeUpdate,
		Revision: 1,
		Mappings: nil,
	}))
	msg, err = control.ReadMessage(conn)
	require.NoError(t, err)
	ack, ok := msg.(*control.UpdateAck)
	require.True(t, ok)
	require.Equal(t, int64(1), ack.Revision)
	require.Empty(t, ack.Results)

	client, ok := srv.Client("legacy-clear-client")
	require.True(t, ok)
	require.Empty(t, client.Mappings)
	require.Empty(t, srv.store.Snapshot().Clients["legacy-clear-client"].Mappings)

	// The public listener must be closed, not merely removed from the view.
	_, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", assignedPort), 200*time.Millisecond)
	require.Error(t, err, "clearing the last mapping must close its public listener")
}

func TestRegistrationRacingRevokeIsRejected(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	_, _, err := srv.RotateCredentials("new-password", "new-token-0123456789abcdef", "manual")
	require.NoError(t, err)

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	// Hold the mapping lock so registration cannot finish between
	// authentication and the revoke sweep.
	srv.mappings.mu.Lock()
	locked := true
	defer func() {
		if locked {
			srv.mappings.mu.Unlock()
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		newControlHandler(srv, srv.log).Serve(serverConn)
	}()

	// net.Pipe writes complete only after the handler reads the hello, so this
	// returns once authentication is underway and registration is blocked.
	require.NoError(t, control.WriteMessage(clientConn, &control.Hello{
		Type:                 control.TypeHello,
		Protocol:             control.ProtocolVersion,
		Token:                defaultTestToken,
		ClientID:             "racing-client",
		Name:                 "racing",
		Version:              "test",
		Revision:             0,
		CredentialGeneration: 2,
	}))
	time.Sleep(20 * time.Millisecond)

	revokeDone := make(chan error, 1)
	go func() { revokeDone <- srv.RevokePreviousCredentials() }()
	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		return srv.CredentialsSnapshot().PreviousGeneration == 0
	}))

	srv.mappings.mu.Unlock()
	locked = false
	require.NoError(t, <-revokeDone)

	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = control.ReadMessage(clientConn)
	require.Error(t, err, "a hello racing revoke must not receive a hello_ack")
	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		view, ok := srv.Client("racing-client")
		return !ok || !view.Online
	}))
	<-done
}

func TestServerViews(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		return len(pair.Server.Clients()) == 1
	}))

	views := pair.Server.Clients()
	id := views[0].ID
	view, ok := pair.Server.Client(id)
	require.True(t, ok)
	require.Equal(t, id, view.ID)
	require.True(t, view.Online)

	_, ok = pair.Server.Client("missing")
	require.False(t, ok)

	overview := pair.Server.Overview()
	require.Equal(t, 1, overview.OnlineClients)
	require.Equal(t, 1, overview.ActiveMappings)
	require.NotEmpty(t, overview.Listen)
	require.Positive(t, overview.Uptime)

	_, err := pair.Server.PushMappings("missing", 0, nil)
	require.ErrorIs(t, err, control.ErrClientOffline)
}

func TestClientRejectsWrongControlToken(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	cfg := testClientConfig(srv.ListenAddr().String(), []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	cfg.Control.Token = strings.Repeat("f", 32)
	cl, err := client.New(cfg, slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	go func() { _ = cl.Start(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	require.Error(t, cl.WaitReady(ctx))
	require.Empty(t, srv.Clients())
}

func TestControlRejectsNonHelloFirstMessage(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	hy := newHY2Client(t, srv, "pw", nil)
	conn, err := hy.TCP("_hy2tun_control_:0")
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, control.WriteMessage(conn, &control.Ping{
		Type: control.TypePing,
		TS:   1,
	}))
	msg, err := control.ReadMessage(conn)
	require.NoError(t, err)
	ack, ok := msg.(*control.HelloAck)
	require.True(t, ok)
	require.False(t, ack.OK)
	require.Equal(t, "bad_message", ack.Error)
	require.Empty(t, srv.Clients())
}

func TestClientRegistersThroughSalamanderObfs(t *testing.T) {
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	serverCfg := validServerConfig(cert)
	serverCfg.Obfs = config.ObfsConfig{
		Type:       "salamander",
		Salamander: config.SalamanderConfig{Password: "obfs-secret"},
	}
	srv, stop := startServer(t, serverCfg)
	defer stop()

	cfg := testClientConfig(srv.ListenAddr().String(), []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	cfg.Obfs = serverCfg.Obfs
	cl, err := client.New(cfg, slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	go func() { _ = cl.Start(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, cl.WaitReady(ctx))
	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		views := srv.Clients()
		return len(views) == 1 && len(views[0].Mappings) == 1 && views[0].Mappings[0].Status == "running"
	}))
}

func TestLoadedOnlineClientBecomesOfflineAndReservesPort(t *testing.T) {
	const reservedPort = 23457
	statePath := filepath.Join(t.TempDir(), "state.json")

	store, err := state.Open(statePath, 24*time.Hour)
	require.NoError(t, err)
	store.Upsert(state.ClientState{
		ID:       "saved-client",
		Name:     "saved",
		Version:  "test",
		Revision: 3,
		Online:   true,
		LastSeen: time.Now(),
		Mappings: []config.Mapping{
			{ID: "m1", Type: "tcp", RemotePort: reservedPort, LocalAddr: "127.0.0.1:9001", Enabled: true},
		},
	})
	require.NoError(t, store.Save())
	require.NoError(t, store.Close())

	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	serverCfg := validServerConfig(cert)
	serverCfg.StateFile = statePath
	serverCfg.Ports.AutoRange = fmt.Sprintf("%d", reservedPort)
	srv, stop := startServer(t, serverCfg)
	defer stop()

	views := srv.Clients()
	require.Len(t, views, 1)
	require.Equal(t, "saved-client", views[0].ID)
	require.False(t, views[0].Online)

	cfg := testClientConfig(srv.ListenAddr().String(), []config.Mapping{
		{ID: "auto", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true},
	})
	cl, err := client.New(cfg, slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	go func() { _ = cl.Start(context.Background()) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, cl.WaitReady(ctx))

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		for _, view := range srv.Clients() {
			if view.ID != cfg.Client.ID {
				continue
			}
			return len(view.Mappings) == 1 &&
				view.Mappings[0].Status == "failed" &&
				view.Mappings[0].Error == "no_free_port"
		}
		return false
	}))
}

func TestTwoClientsRacingForSamePort(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	const port = 23456
	var (
		clientsMu sync.Mutex
		clients   []*client.Client
	)
	defer func() {
		clientsMu.Lock()
		defer clientsMu.Unlock()
		for _, cl := range clients {
			_ = cl.Close()
		}
	}()
	for i := 0; i < 2; i++ {
		go func(i int) {
			cfg := testClientConfig(srv.ListenAddr().String(), []config.Mapping{
				{ID: "same", Type: "tcp", RemotePort: port, LocalAddr: "127.0.0.1:9001", Enabled: true},
			})
			cfg.Client.Name = fmt.Sprintf("c%d", i)
			cl, err := client.New(cfg, slog.Default())
			if err != nil {
				return
			}
			clientsMu.Lock()
			clients = append(clients, cl)
			clientsMu.Unlock()
			go func() { _ = cl.Start(context.Background()) }()
			_ = cl.WaitReady(context.Background())
		}(i)
	}

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		return len(srv.Clients()) == 2
	}))

	running, failed := 0, 0
	for _, c := range srv.Clients() {
		for _, m := range c.Mappings {
			switch m.Status {
			case "running":
				running++
			case "failed":
				failed++
				require.Equal(t, "port_in_use", m.Error)
			}
		}
	}
	require.Equal(t, 1, running)
	require.Equal(t, 1, failed)
}
