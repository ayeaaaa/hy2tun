package server

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/testutil"
	"hy2tun/internal/view"
)

const (
	defaultTestAuth  = "pw"
	defaultTestToken = "0123456789abcdef0123456789abcdef"
)

// openCredentialControl opens a raw control stream with an explicit credential
// generation and capability. It lets rotation tests drive credential_ack and
// credential_update directly without depending on the real client.
func openCredentialControl(t *testing.T, srv *Server, auth, token, clientID string, generation int64, capable bool) net.Conn {
	t.Helper()

	hy := newHY2Client(t, srv, auth, nil)
	conn := openRawControl(t, hy)

	hello := &control.Hello{
		Type:                 control.TypeHello,
		Protocol:             control.ProtocolVersion,
		Token:                token,
		ClientID:             clientID,
		Name:                 clientID,
		Version:              "test",
		Revision:             0,
		CredentialGeneration: generation,
	}
	if capable {
		hello.Capabilities = []string{control.CapabilityCredentialUpdate}
	}
	require.NoError(t, control.WriteMessage(conn, hello))

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := control.ReadMessage(conn)
	require.NoError(t, err)
	_ = conn.SetReadDeadline(time.Time{})
	ack, ok := msg.(*control.HelloAck)
	require.True(t, ok, "expected hello_ack, got %T", msg)
	require.True(t, ack.OK, "hello rejected: %s", ack.Error)

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		_, ok := srv.Client(clientID)
		return ok
	}))
	return conn
}

// readCredentialUpdate reads one credential_update message.
func readCredentialUpdate(t *testing.T, conn net.Conn) *control.CredentialUpdate {
	t.Helper()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := control.ReadMessage(conn)
	require.NoError(t, err)
	_ = conn.SetReadDeadline(time.Time{})
	update, ok := msg.(*control.CredentialUpdate)
	require.True(t, ok, "expected credential_update, got %T", msg)
	return update
}

// writeCredentialAck sends one credential_ack message.
func writeCredentialAck(t *testing.T, conn net.Conn, generation int64, ok bool, code string) {
	t.Helper()

	require.NoError(t, control.WriteMessage(conn, &control.CredentialAck{
		Type:       control.TypeCredentialAck,
		Generation: generation,
		OK:         ok,
		Error:      code,
	}))
}

func TestRotateCredentialsAutoPush(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()
	require.NoError(t, waitFor(t, 5*time.Second, func() bool { return len(pair.Server.Clients()) == 1 }))

	gen, results, err := pair.Server.RotateCredentials("new-auth", "new-control-token", "auto")
	require.NoError(t, err)
	require.Equal(t, int64(2), gen)
	require.Len(t, results, 1)
	require.Equal(t, "synced", results[0].Status)

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && v[0].CredentialGeneration == 2 && v[0].CredentialSynced
	}))
	saved, err := config.LoadClient(pair.ClientConfigPath)
	require.NoError(t, err)
	require.Equal(t, int64(2), saved.Credentials.Generation)
	require.Equal(t, "new-auth", saved.Auth)
	require.Equal(t, "new-control-token", saved.Control.Token)
}

func TestRotateCredentialsManualSync(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()
	require.NoError(t, waitFor(t, 5*time.Second, func() bool { return len(pair.Server.Clients()) == 1 }))

	gen, results, err := pair.Server.RotateCredentials("new-password", "new-control-token", "manual")
	require.NoError(t, err)
	require.Equal(t, int64(2), gen)
	require.Len(t, results, 1)
	require.Equal(t, "pending", results[0].Status)

	snapshot := pair.Server.CredentialsSnapshot()
	require.NotNil(t, snapshot)
	require.Equal(t, int64(2), snapshot.Generation)
	require.Equal(t, "new-password", snapshot.Auth)
	require.Equal(t, "new-control-token", snapshot.ControlToken)
	require.Equal(t, int64(1), snapshot.PreviousGeneration)
	require.Equal(t, defaultTestAuth, snapshot.PreviousAuth)
	require.Equal(t, defaultTestToken, snapshot.PreviousControlToken)
	require.True(t, snapshot.AcceptAuth(defaultTestAuth))
	require.True(t, snapshot.AcceptAuth("new-password"))
	previousGen, ok := snapshot.AcceptControlToken(defaultTestToken)
	require.True(t, ok)
	require.Equal(t, int64(1), previousGen)
	currentGen, ok := snapshot.AcceptControlToken("new-control-token")
	require.True(t, ok)
	require.Equal(t, int64(2), currentGen)

	// A copy escapes the store without aliasing it.
	snapshot.Auth = "mutated"
	require.NotEqual(t, "mutated", pair.Server.CredentialsSnapshot().Auth)

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && v[0].CredentialGeneration == 1 && !v[0].CredentialSynced
	}))

	statuses := pair.Server.CredentialStatuses()
	require.Len(t, statuses, 1)
	require.Equal(t, "pending", statuses[0].Status)
}

func TestSingleFieldRotation(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	gen, _, err := srv.RotateCredentials("new-password", "", "manual")
	require.NoError(t, err)
	require.Equal(t, int64(2), gen)

	snapshot := srv.CredentialsSnapshot()
	require.Equal(t, "new-password", snapshot.Auth)
	require.Equal(t, defaultTestToken, snapshot.ControlToken)
	require.Equal(t, int64(1), snapshot.PreviousGeneration)
	require.Equal(t, defaultTestAuth, snapshot.PreviousAuth)
	require.Equal(t, defaultTestToken, snapshot.PreviousControlToken)

	// Rotating only the token keeps the current password and snapshots the old
	// current pair, not the earlier generation.
	gen, _, err = srv.RotateCredentials("", "new-token-0123456789abcdef", "manual")
	require.NoError(t, err)
	require.Equal(t, int64(3), gen)

	snapshot = srv.CredentialsSnapshot()
	require.Equal(t, "new-password", snapshot.Auth)
	require.Equal(t, "new-token-0123456789abcdef", snapshot.ControlToken)
	require.Equal(t, int64(2), snapshot.PreviousGeneration)
	require.Equal(t, "new-password", snapshot.PreviousAuth)
	require.Equal(t, defaultTestToken, snapshot.PreviousControlToken)

	// No-op rotations must not create a generation.
	for _, tc := range []struct {
		name  string
		auth  string
		token string
	}{
		{name: "both empty"},
		{name: "auth unchanged", auth: "new-password"},
		{name: "token unchanged", token: "new-token-0123456789abcdef"},
		{name: "both unchanged", auth: "new-password", token: "new-token-0123456789abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := srv.RotateCredentials(tc.auth, tc.token, "manual")
			require.ErrorIs(t, err, ErrNoCredentialChange)
			require.Equal(t, int64(3), srv.CredentialsSnapshot().Generation)
		})
	}

	// Only the changed field is validated for length.
	_, _, err = srv.RotateCredentials("short", "", "manual")
	require.Error(t, err)
	_, _, err = srv.RotateCredentials("", "short", "manual")
	require.Error(t, err)
	require.Equal(t, int64(3), srv.CredentialsSnapshot().Generation)
}

func TestRotateCredentialsUnsupportedAuthType(t *testing.T) {
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	cfg.Auth = config.AuthConfig{
		Type:  "userpass",
		Users: map[string]string{"user": "password"},
	}
	srv, stop := startServer(t, cfg)
	defer stop()

	_, _, err = srv.RotateCredentials("new-password", "", "manual")
	require.ErrorIs(t, err, ErrUnsupportedAuthType)
	require.Equal(t, int64(1), srv.CredentialsSnapshot().Generation)
}

func TestRotateCredentialsAutoPushTimeout(t *testing.T) {
	oldTimeout := credentialAckTimeout
	credentialAckTimeout = 100 * time.Millisecond
	defer func() { credentialAckTimeout = oldTimeout }()

	srv, stop := startTestServer(t)
	defer stop()

	conn := openCredentialControl(t, srv, defaultTestAuth, defaultTestToken, "silent-client", 1, true)
	defer conn.Close()

	gen, results, err := srv.RotateCredentials("new-password", "", "auto")
	require.NoError(t, err)
	require.Equal(t, int64(2), gen)
	require.Len(t, results, 1)
	require.Equal(t, "timeout", results[0].Status)

	// The client remains on the previous generation and is shown as pending.
	view, ok := srv.Client("silent-client")
	require.True(t, ok)
	require.Equal(t, int64(1), view.CredentialGeneration)
	require.False(t, view.CredentialSynced)

	statuses := srv.CredentialStatuses()
	require.Len(t, statuses, 1)
	require.Equal(t, "pending", statuses[0].Status)
}

func TestRotateCredentialsAutoPushIgnoresStaleAck(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	conn := openCredentialControl(t, srv, defaultTestAuth, defaultTestToken, "stale-ack-client", 1, true)
	defer conn.Close()

	// A stale ack from an earlier push must not satisfy this rotation.
	writeCredentialAck(t, conn, 99, true, "")

	type rotationResult struct {
		gen     int64
		results []view.CredentialResult
		err     error
	}
	resultCh := make(chan rotationResult, 1)
	go func() {
		gen, results, err := srv.RotateCredentials("new-password", "", "auto")
		resultCh <- rotationResult{gen: gen, results: results, err: err}
	}()

	update := readCredentialUpdate(t, conn)
	require.Equal(t, int64(2), update.Generation)
	require.Equal(t, "new-password", update.Auth)
	require.Equal(t, defaultTestToken, update.ControlToken)
	writeCredentialAck(t, conn, update.Generation, true, "")

	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		require.Equal(t, int64(2), result.gen)
		require.Len(t, result.results, 1)
		require.Equal(t, "synced", result.results[0].Status)
	case <-time.After(5 * time.Second):
		t.Fatal("rotation did not return")
	}
}

func TestRotateCredentialsAutoPushMarksIncapableUnsupported(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	conn := openCredentialControl(t, srv, defaultTestAuth, defaultTestToken, "legacy-client", 1, false)
	defer conn.Close()

	gen, results, err := srv.RotateCredentials("new-password", "", "auto")
	require.NoError(t, err)
	require.Equal(t, int64(2), gen)
	require.Len(t, results, 1)
	require.Equal(t, "unsupported", results[0].Status)

	statuses := srv.CredentialStatuses()
	require.Len(t, statuses, 1)
	require.Equal(t, "unsupported", statuses[0].Status)
}

func TestRevokePreviousCredentials(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	conn := openCredentialControl(t, srv, defaultTestAuth, defaultTestToken, "old-client", 1, true)
	defer conn.Close()

	_, _, err := srv.RotateCredentials("new-password", "new-token-0123456789abcdef", "manual")
	require.NoError(t, err)
	require.Equal(t, int64(1), srv.CredentialsSnapshot().PreviousGeneration)

	require.NoError(t, srv.RevokePreviousCredentials())

	snapshot := srv.CredentialsSnapshot()
	require.Equal(t, int64(2), snapshot.Generation)
	require.Equal(t, "new-password", snapshot.Auth)
	require.Equal(t, "new-token-0123456789abcdef", snapshot.ControlToken)
	require.Equal(t, int64(0), snapshot.PreviousGeneration)
	require.Empty(t, snapshot.PreviousAuth)
	require.Empty(t, snapshot.PreviousControlToken)
	require.False(t, snapshot.AcceptAuth(defaultTestAuth))
	require.True(t, snapshot.AcceptAuth("new-password"))
	_, ok := snapshot.AcceptControlToken(defaultTestToken)
	require.False(t, ok)

	// The old session is closed so it cannot keep using revoked credentials.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = control.ReadMessage(conn)
	require.Error(t, err)
	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		view, ok := srv.Client("old-client")
		return !ok || !view.Online
	}))

	// Revoking again is a no-op.
	require.NoError(t, srv.RevokePreviousCredentials())
}

func TestRevokePreviousCredentialsClosesOnlyStaleSessions(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	oldConn := openCredentialControl(t, srv, defaultTestAuth, defaultTestToken, "old-client", 1, true)
	defer oldConn.Close()

	_, _, err := srv.RotateCredentials("new-password", "", "manual")
	require.NoError(t, err)

	// A client that manually caught up to the current generation survives the
	// revoke; only sessions with an older generation are closed.
	currentConn := openCredentialControl(t, srv, "new-password", defaultTestToken, "current-client", 2, true)
	defer currentConn.Close()

	require.NoError(t, srv.RevokePreviousCredentials())

	_ = oldConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = control.ReadMessage(oldConn)
	require.Error(t, err)

	currentView, ok := srv.Client("current-client")
	require.True(t, ok)
	require.True(t, currentView.Online)
}

func TestSecondRotationClosesSupersededSessions(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	conn := openCredentialControl(t, srv, defaultTestAuth, defaultTestToken, "superseded-client", 1, true)
	defer conn.Close()

	_, _, err := srv.RotateCredentials("new-password-1", "", "manual")
	require.NoError(t, err)
	_, _, err = srv.RotateCredentials("new-password-2", "", "manual")
	require.NoError(t, err)

	snapshot := srv.CredentialsSnapshot()
	require.Equal(t, int64(3), snapshot.Generation)
	require.Equal(t, "new-password-2", snapshot.Auth)
	require.Equal(t, int64(2), snapshot.PreviousGeneration)
	require.Equal(t, "new-password-1", snapshot.PreviousAuth)

	// Generation 1 is neither the current nor the previous generation.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = control.ReadMessage(conn)
	require.Error(t, err)
}

func TestRotateCredentialsWriteFailureKeepsRuntime(t *testing.T) {
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	cfg.ConfigPath = filepath.Join(t.TempDir(), "server.yaml")

	srv, stop := startServer(t, cfg)
	defer stop()

	writeErr := errors.New("write failed")
	srv.saveConfig = func(string, *config.ServerConfig) error { return writeErr }

	_, _, err = srv.RotateCredentials("new-password", "", "manual")
	require.Error(t, err)
	require.ErrorIs(t, err, writeErr)

	snapshot := srv.CredentialsSnapshot()
	require.Equal(t, int64(1), snapshot.Generation)
	require.Equal(t, defaultTestAuth, snapshot.Auth)
	require.Equal(t, int64(0), snapshot.PreviousGeneration)
	require.Equal(t, defaultTestAuth, srv.currentConfig().Auth.Password)
}

func TestRevokePreviousCredentialsWriteFailureKeepsRuntime(t *testing.T) {
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	dir := t.TempDir()
	cfg.ConfigPath = filepath.Join(dir, "server.yaml")
	require.NoError(t, config.SaveServer(cfg.ConfigPath, cfg))

	srv, stop := startServer(t, cfg)
	defer stop()

	_, _, err = srv.RotateCredentials("new-password", "", "manual")
	require.NoError(t, err)
	require.Equal(t, int64(1), srv.CredentialsSnapshot().PreviousGeneration)

	writeErr := errors.New("write failed")
	srv.saveConfig = func(string, *config.ServerConfig) error { return writeErr }

	require.ErrorIs(t, srv.RevokePreviousCredentials(), writeErr)
	snapshot := srv.CredentialsSnapshot()
	require.Equal(t, int64(1), snapshot.PreviousGeneration)
	require.Equal(t, defaultTestAuth, snapshot.PreviousAuth)
}

func TestRotateCredentialsPersistsConfig(t *testing.T) {
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	dir := t.TempDir()
	cfg.ConfigPath = filepath.Join(dir, "server.yaml")
	require.NoError(t, config.SaveServer(cfg.ConfigPath, cfg))

	srv, stop := startServer(t, cfg)
	defer stop()

	_, _, err = srv.RotateCredentials("new-password", "new-token-0123456789abcdef", "manual")
	require.NoError(t, err)

	saved, err := config.LoadServer(cfg.ConfigPath)
	require.NoError(t, err)
	require.Equal(t, "new-password", saved.Auth.Password)
	require.Equal(t, "new-token-0123456789abcdef", saved.Control.Token)
	require.Equal(t, int64(2), saved.Credentials.Generation)
	require.Equal(t, int64(1), saved.Credentials.PreviousGeneration)
	require.Equal(t, defaultTestAuth, saved.Credentials.PreviousAuth)
	require.Equal(t, defaultTestToken, saved.Credentials.PreviousControlToken)

	info, err := os.Stat(cfg.ConfigPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	_, err = os.Stat(cfg.ConfigPath + ".bak")
	require.NoError(t, err)

	require.NoError(t, srv.RevokePreviousCredentials())
	saved, err = config.LoadServer(cfg.ConfigPath)
	require.NoError(t, err)
	require.Equal(t, int64(2), saved.Credentials.Generation)
	require.Equal(t, int64(0), saved.Credentials.PreviousGeneration)
	require.Empty(t, saved.Credentials.PreviousAuth)
	require.Empty(t, saved.Credentials.PreviousControlToken)
}

func TestRevokedCredentialsAreRejectedByRuntime(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	_, _, err := srv.RotateCredentials("new-password", "", "manual")
	require.NoError(t, err)
	require.NoError(t, srv.RevokePreviousCredentials())

	_, _, err = hyclient.NewClient(&hyclient.Config{
		ServerAddr:  mustAddr(srv.ListenAddr().String()),
		Auth:        defaultTestAuth,
		TLSConfig:   hyclient.TLSConfig{InsecureSkipVerify: true},
		ConnFactory: nil,
	})
	require.Error(t, err)
}
