package client

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/config"
)

func TestClientWebConsoleStartsAndPersistsSecret(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	require.NoError(t, err)

	dir := t.TempDir()
	path := filepath.Join(dir, "client.yaml")
	cfg := validTestClientConfig()
	cfg.Web = config.WebConfig{
		Listen:       "127.0.0.1:0",
		Username:     "admin",
		PasswordHash: string(hash),
	}
	require.NoError(t, config.SaveClient(path, cfg))

	loaded, err := config.LoadClient(path)
	require.NoError(t, err)
	cl, err := New(loaded, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = cl.Close() })

	secretPath := config.WebSecretPath(path)
	info, err := os.Stat(secretPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cl.Start(ctx) }()

	require.Eventually(t, func() bool {
		return cl.WebAddr() != "" && cl.WebAddr() != "127.0.0.1:0"
	}, 2*time.Second, 10*time.Millisecond)

	resp, err := http.Get("http://" + cl.WebAddr() + "/login")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("client Start did not return after cancellation")
	}
	require.NoError(t, cl.Close())

	// The secret is reused on restart.
	reloaded, err := config.LoadOrCreateWebSecret(secretPath)
	require.NoError(t, err)
	require.Len(t, reloaded, 32)
}
