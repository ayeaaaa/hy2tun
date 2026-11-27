package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/manager"
)

const (
	testAdminPassword    = "old-test-admin-password"
	testNewAdminPassword = "new-test-admin-password"
)

func TestRunServesManagerAndStopsOnSIGTERM(t *testing.T) {
	dir := t.TempDir()
	statePath, keyPath := writeManagerState(t, dir, testAdminPassword)
	configPath := writeManagerConfig(t, dir, "127.0.0.1:0")

	before, err := manager.OpenStore(statePath, keyPath)
	require.NoError(t, err)
	beforeState := before.State()

	// Normal startup must not consume reset input or change the administrator.
	stdin := strings.NewReader(testNewAdminPassword + "\n" + testNewAdminPassword + "\n")
	ready := make(chan net.Addr, 1)
	runErr := make(chan error, 1)
	go func() {
		runErr <- run([]string{"-config", configPath}, stdin, io.Discard, io.Discard, func(addr net.Addr) {
			ready <- addr
		})
	}()

	var addr net.Addr
	select {
	case addr = <-ready:
	case err := <-runErr:
		t.Fatalf("manager exited before listening: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for manager to listen")
	}

	// Only install the emergency SIGTERM cleanup after the signal handler has
	// definitely been registered (readiness happens after signal.NotifyContext).
	finished := false
	defer func() {
		if finished {
			return
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case <-runErr:
		case <-time.After(10 * time.Second):
		}
	}()

	resp, err := http.Get("http://" + addr.String() + "/login")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), "hy2tun 管理器")

	require.Equal(t, len(testNewAdminPassword)*2+2, stdin.Len(),
		"normal startup must not read reset-admin input")

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
	select {
	case err := <-runErr:
		finished = true
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("manager did not shut down after SIGTERM")
	}

	after, err := manager.OpenStore(statePath, keyPath)
	require.NoError(t, err)
	require.Equal(t, beforeState.Admin.PasswordHash, after.State().Admin.PasswordHash,
		"normal startup must not reset the administrator password")
}

func TestResetAdminChangesOnlyPasswordAndInvalidatesOldSession(t *testing.T) {
	dir := t.TempDir()
	statePath, keyPath := writeManagerState(t, dir, testAdminPassword)
	configPath := writeManagerConfig(t, dir, "127.0.0.1:0")

	store, err := manager.OpenStore(statePath, keyPath)
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, store.AddServer(manager.ServerRecord{
		ID:               "srv_test",
		Name:             "test",
		BaseURL:          "https://server.example.test/hy2tund",
		UpstreamUsername: "admin",
		CreatedAt:        now,
		UpdatedAt:        now,
	}, []byte("upstream-test-password")))
	before := store.State()

	oldWeb, err := manager.NewWebServer(store, manager.NewAuditLog(filepath.Join(dir, "audit.jsonl")),
		manager.Config{BasePath: "/"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	oldTS := httptest.NewServer(oldWeb.Handler())
	oldResp, err := oldTS.Client().PostForm(oldTS.URL+"/api/login", url.Values{
		"username": {"admin"}, "password": {testAdminPassword},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, oldResp.StatusCode)
	require.NoError(t, oldResp.Body.Close())
	oldCookie := oldResp.Cookies()[0]
	oldTS.Close()

	var stderr bytes.Buffer
	err = run([]string{"-config", configPath, "-reset-admin"},
		strings.NewReader(testNewAdminPassword+"\n"+testNewAdminPassword+"\n"),
		io.Discard, &stderr, nil)
	require.NoError(t, err)
	require.NotContains(t, stderr.String(), testAdminPassword)
	require.NotContains(t, stderr.String(), testNewAdminPassword)

	reopened, err := manager.OpenStore(statePath, keyPath)
	require.NoError(t, err)
	after := reopened.State()
	require.NotEqual(t, before.Admin.PasswordHash, after.Admin.PasswordHash)
	require.NotEqual(t, testAdminPassword, testNewAdminPassword)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(after.Admin.PasswordHash), []byte(testNewAdminPassword)))
	require.Error(t, bcrypt.CompareHashAndPassword([]byte(after.Admin.PasswordHash), []byte(testAdminPassword)))

	// Only the bcrypt hash may change: username, servers, and key check survive.
	before.Admin.PasswordHash = ""
	after.Admin.PasswordHash = ""
	require.Equal(t, before, after)

	// A restarted manager uses the new hash; the old password and the old
	// process's session cookie are both rejected.
	newWeb, err := manager.NewWebServer(reopened, manager.NewAuditLog(filepath.Join(dir, "audit.jsonl")),
		manager.Config{BasePath: "/"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	newTS := httptest.NewServer(newWeb.Handler())
	defer newTS.Close()
	noRedirect := newTS.Client()
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	req, err := http.NewRequest(http.MethodGet, newTS.URL+"/", nil)
	require.NoError(t, err)
	req.AddCookie(oldCookie)
	resp, err := noRedirect.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	require.Equal(t, "/login", resp.Header.Get("Location"))

	resp, err = newTS.Client().PostForm(newTS.URL+"/api/login", url.Values{
		"username": {"admin"}, "password": {testAdminPassword},
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	resp, err = newTS.Client().PostForm(newTS.URL+"/api/login", url.Values{
		"username": {"admin"}, "password": {testNewAdminPassword},
	})
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestReleaseManagerDryRun(t *testing.T) {
	root := repositoryRoot(t)
	dist := t.TempDir()
	cmd := exec.Command("sh", filepath.Join(root, "scripts", "release-manager.sh"), "--dry-run")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "VERSION=test", "DIST="+dist)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "release-manager dry-run failed:\n%s", output)
	text := string(output)
	require.Contains(t, text, "hy2tun-manager_test_linux_amd64.tar.gz")
	require.Contains(t, text, "hy2tun-manager_test_linux_arm64.tar.gz")
	require.NotContains(t, text, "hy2tun_test_linux_amd64")

	entries, err := os.ReadDir(dist)
	require.NoError(t, err)
	require.Empty(t, entries, "dry-run must not build or write archives")
}

func TestReleaseManagerArchives(t *testing.T) {
	if testing.Short() {
		t.Skip("release archive build crosses two architectures")
	}
	root := repositoryRoot(t)
	dist := t.TempDir()
	cmd := exec.Command("sh", filepath.Join(root, "scripts", "release-manager.sh"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "VERSION=test", "DIST="+dist)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "release-manager build failed:\n%s", output)

	for _, arch := range []string{"amd64", "arm64"} {
		archive := filepath.Join(dist, fmt.Sprintf("hy2tun-manager_test_linux_%s.tar.gz", arch))
		require.FileExists(t, archive)
		listing, err := exec.Command("tar", "-tzf", archive).CombinedOutput()
		require.NoError(t, err, "list archive %s: %s", archive, listing)
		names := make(map[string]bool)
		for _, line := range strings.Split(strings.TrimSpace(string(listing)), "\n") {
			names[strings.TrimPrefix(strings.TrimSpace(line), "./")] = true
		}
		for _, want := range []string{"hy2tun-manager", "manager.example.yaml", "hy2tun-manager.service", "README.md"} {
			require.True(t, names[want], "%s is missing from %s (got %v)", want, filepath.Base(archive), names)
		}
		require.False(t, names["hy2tund"], "manager archive must not contain server binaries")
		require.False(t, names["hy2tun"], "manager archive must not contain client binaries")
	}
}

func writeManagerState(t *testing.T, dir, password string) (statePath, keyPath string) {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	keyPath = filepath.Join(dir, "manager.key")
	statePath = filepath.Join(dir, "state.json")
	require.NoError(t, os.WriteFile(keyPath, []byte(hex.EncodeToString(key)+"\n"), 0o600))

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	state := manager.State{
		Admin:   manager.AdminRecord{Username: "admin", PasswordHash: string(hash)},
		Servers: []manager.ServerRecord{},
	}
	data, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, append(data, '\n'), 0o600))
	return statePath, keyPath
}

func writeManagerConfig(t *testing.T, dir, listen string) string {
	t.Helper()
	path := filepath.Join(dir, "manager.yaml")
	content := fmt.Sprintf(`listen: %q
base_path: "/"
state_file: %q
key_file: %q
audit_file: %q
refresh_interval: "1s"
trust_proxy: false
`, listen, filepath.Join(dir, "state.json"), filepath.Join(dir, "manager.key"), filepath.Join(dir, "audit.jsonl"))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}
