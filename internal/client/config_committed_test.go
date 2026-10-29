package client

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
)

func TestClientWriteConfigTreatsCommittedAsSuccess(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "client.yaml")
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer cl.Close()

	cl.SetPersist(func(*config.ClientConfig) error {
		return fmt.Errorf("%w: sync failed", config.ErrCommitted)
	})
	require.NoError(t, cl.UpdateName("committed-name"))
	require.Equal(t, "committed-name", cl.snapshotConfig().Client.Name)
}

func TestTCPContextCancelsBlockedDial(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()
	fake := newStalledHY2Client()
	cl.mu.Lock()
	cl.hy = fake
	cl.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		conn, err := cl.TCPContext(ctx, "127.0.0.1:1")
		if conn != nil {
			_ = conn.Close()
		}
		done <- err
	}()
	select {
	case <-fake.tcpStarted:
	case <-time.After(time.Second):
		t.Fatal("dial did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("TCPContext did not honor cancellation")
	}
	_ = fake.Close()
}
