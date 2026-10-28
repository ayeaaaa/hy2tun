package client

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
)

// TestCredentialUpdateForcesReconnect verifies that the client applies a
// pushed credential pair, acks it, and then closes the control stream even when
// the server keeps the stream open. Closing is what makes runOnce reconnect
// with the new generation.
func TestCredentialUpdateForcesReconnect(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 1)
	cl, err := New(loadClientConfig(t, cfgPath), nil)
	require.NoError(t, err)
	defer cl.Close()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		cl.handleCredentialUpdate(clientConn, &control.CredentialUpdate{
			Type:         control.TypeCredentialUpdate,
			Generation:   2,
			Auth:         "new-password",
			ControlToken: "new-token-0123456789abcdef",
		})
	}()

	// Read the ack before the client's grace period closes the stream.
	_ = serverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	msg, err := control.ReadMessage(serverConn)
	require.NoError(t, err)
	ack, ok := msg.(*control.CredentialAck)
	require.True(t, ok, "expected credential_ack, got %T", msg)
	require.True(t, ack.OK)
	require.Equal(t, int64(2), ack.Generation)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("credential update handler did not return")
	}

	// The fake server never closes its side. The client must still close the
	// stream so the reconnect loop can send a new Hello.
	readResult := make(chan error, 1)
	go func() {
		_ = serverConn.SetReadDeadline(time.Now().Add(3 * time.Second))
		_, readErr := serverConn.Read(make([]byte, 1))
		readResult <- readErr
	}()
	select {
	case readErr := <-readResult:
		require.Error(t, readErr, "client did not close the control stream")
	case <-time.After(2 * time.Second):
		t.Fatal("client did not close the control stream")
	}

	saved, err := config.LoadClient(cfgPath)
	require.NoError(t, err)
	require.Equal(t, int64(2), saved.Credentials.Generation)
	require.Equal(t, "new-password", saved.Auth)
	require.Equal(t, "new-token-0123456789abcdef", saved.Control.Token)
}
