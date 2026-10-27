package server

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/config"
	"hy2tun/internal/testutil"
)

func TestServerWebConsoleStartsAndPersistsSecret(t *testing.T) {
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	dir := t.TempDir()
	cfg.ConfigPath = filepath.Join(dir, "server.yaml")

	hash, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	require.NoError(t, err)
	cfg.Web = config.WebConfig{
		Listen:       "127.0.0.1:0",
		Username:     "admin",
		PasswordHash: string(hash),
	}

	srv, err := New(cfg, slog.Default())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(ctx) }()
	defer srv.Close()

	require.Eventually(t, func() bool {
		addr := srv.WebAddr()
		return addr != "" && addr != cfg.Web.Listen
	}, 2*time.Second, 10*time.Millisecond)
	select {
	case err := <-startErr:
		t.Fatalf("server stopped before the web console became reachable: %v", err)
	default:
	}

	resp, err := http.Get("http://" + srv.WebAddr() + "/login")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	secretPath := filepath.Join(dir, "web_secret")
	info, err := os.Stat(secretPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	first, err := os.ReadFile(secretPath)
	require.NoError(t, err)
	require.Len(t, strings.TrimSpace(string(first)), 64)

	// A restart loads the same key instead of generating a new one.
	key, err := config.LoadOrCreateWebSecret(secretPath)
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(string(first)), hex.EncodeToString(key))
}
