package server

import (
	"context"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestUpdateWebConfigHotUpdatesWebLogin(t *testing.T) {
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	dir := t.TempDir()
	cfg.ConfigPath = filepath.Join(dir, "server.yaml")

	oldHash, err := bcrypt.GenerateFromPassword([]byte("old-password"), bcrypt.MinCost)
	require.NoError(t, err)
	newHash, err := bcrypt.GenerateFromPassword([]byte("new-password"), bcrypt.MinCost)
	require.NoError(t, err)
	cfg.Web = config.WebConfig{
		Listen:       "127.0.0.1:0",
		Username:     "admin",
		PasswordHash: string(oldHash),
	}

	srv, err := New(cfg, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	require.NotNil(t, srv.web)

	ts := httptest.NewServer(srv.web.Handler())
	t.Cleanup(ts.Close)

	login := func(username, password string) (*http.Response, error) {
		return ts.Client().PostForm(ts.URL+"/api/login", url.Values{
			"username": {username},
			"password": {password},
		})
	}

	resp, err := login("admin", "old-password")
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.NotEmpty(t, resp.Cookies())
	cookie := resp.Cookies()[0]
	resp.Body.Close()

	require.NoError(t, srv.UpdateWebConfig("root", string(newHash)))

	// The captured session cookie is revoked immediately.
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/settings", nil)
	require.NoError(t, err)
	req.AddCookie(cookie)
	resp, err = ts.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// The old credentials no longer work and the new ones do.
	resp, err = login("admin", "old-password")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
	resp, err = login("root", "new-password")
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()

	// The new username and hash were persisted atomically.
	loaded, err := config.LoadServer(cfg.ConfigPath)
	require.NoError(t, err)
	require.Equal(t, "root", loaded.Web.Username)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(loaded.Web.PasswordHash), []byte("new-password")))
}
