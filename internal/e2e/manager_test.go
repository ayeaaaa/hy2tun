package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/client"
	"hy2tun/internal/config"
	"hy2tun/internal/manager"
	"hy2tun/internal/manager/upstream"
	"hy2tun/internal/server"
	"hy2tun/internal/testutil"
	"hy2tun/internal/view"
)

const (
	e2eManagerAdminPassword  = "manager-e2e-admin-password"
	e2eManagerNewWebPassword = "manager-e2e-new-web-password"
	e2eManagerNewAuth        = "manager-e2e-new-auth-password"
	e2eManagerNewToken       = "manager-e2e-new-control-token-0123456789"
)

// TestManagerControlsRealServerEndToEnd runs a real hy2tund and a real hy2tun
// client, then drives the manager through its own authenticated HTTP API. The
// manager reaches the server through the production upstream adapter and
// exercises login, inventory/overview, client detail, a mapping push, offline
// exposure, offline mapping rejection, settings, Web-admin rotation, and HY2
// credential rotation. Nothing in the server or client runtime is replaced by
// an httptest fake.
func TestManagerControlsRealServerEndToEnd(t *testing.T) {
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)

	dir := t.TempDir()
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	certPath, keyPath := writeCertificate(t, dir, cert)

	webHash, err := bcryptHash(e2eWebPassword)
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
			Allow:     []string{"31000-31999"},
			AutoRange: "31000-31999",
		},
		Web: config.WebConfig{
			Listen:       "127.0.0.1:0",
			Username:     "admin",
			PasswordHash: webHash,
		},
		StateFile:  filepath.Join(dir, "server-state.json"),
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
			ID:   "manager-e2e-client",
			Name: "manager-e2e-client",
		},
		Mappings: []config.Mapping{{
			ID:         "manager-e2e-base",
			Type:       "tcp",
			RemotePort: 0,
			LocalAddr:  tcpEchoAddr,
			Note:       "initial mapping",
			Enabled:    true,
		}},
		Web: config.WebConfig{Listen: ""},
		Log: config.LogConfig{Level: "info"},
	}
	require.NoError(t, config.SaveClient(clientConfigPath, clientCfg))
	loadedClientCfg, err := config.LoadClient(clientConfigPath)
	require.NoError(t, err)

	cl, err := client.New(loadedClientCfg, logger)
	require.NoError(t, err)
	clientCtx, stopClient := context.WithCancel(context.Background())
	go func() { _ = cl.Start(clientCtx) }()
	clientClosed := false
	closeClient := func() {
		if clientClosed {
			return
		}
		clientClosed = true
		stopClient()
		_ = cl.Close()
	}
	t.Cleanup(closeClient)

	readyCtx, cancelReady := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelReady()
	require.NoError(t, cl.WaitReady(readyCtx))
	require.Eventually(t, func() bool {
		views := srv.Clients()
		if len(views) != 1 || views[0].Revision == 0 || len(views[0].Mappings) != 1 {
			return false
		}
		return views[0].Mappings[0].Status == "running" && views[0].Mappings[0].AssignedPort != 0
	}, 10*time.Second, 20*time.Millisecond, "real client mapping must become live")

	webAddr := waitForWeb(t, srv)
	managerBase, inventory, managerStore, auditPath := startManagerForE2E(t, dir, logger)
	managerClient := newManagerE2EClient(t, managerBase)

	// Add the real server through the manager's own API. This performs the
	// manager login, upstream login, overview, and config-summary refresh.
	status, body := managerClient.request(t, http.MethodPost, "/api/servers", map[string]any{
		"name":            "e2e-real-server",
		"base_url":        "http://" + webAddr,
		"username":        "admin",
		"password":        e2eWebPassword,
		"tls_skip_verify": false,
	})
	require.Equal(t, http.StatusCreated, status, string(body))
	var serverView manager.ServerView
	require.NoError(t, json.Unmarshal(body, &serverView))
	require.Equal(t, "online", serverView.State)
	require.NotEmpty(t, serverView.Version)
	require.GreaterOrEqual(t, serverView.ClientCount, 1)
	require.GreaterOrEqual(t, serverView.MappingCount, 1)

	status, body = managerClient.request(t, http.MethodGet, "/api/servers", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var fleet struct {
		Servers []manager.ServerView `json:"servers"`
	}
	require.NoError(t, json.Unmarshal(body, &fleet))
	require.Len(t, fleet.Servers, 1)
	require.Equal(t, serverView.ID, fleet.Servers[0].ID)

	status, body = managerClient.request(t, http.MethodGet, "/", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), "e2e-real-server")
	require.Contains(t, string(body), "在线")

	status, body = managerClient.request(t, http.MethodGet, "/servers/"+serverView.ID+"/clients", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), "manager-e2e-client")

	// Client detail comes from the real upstream API. Use the inventory's
	// cached production client rather than duplicating the adapter here.
	var clientView upstream.ClientView
	require.NoError(t, inventory.WithClient(context.Background(), serverView.ID, func(callCtx context.Context, upstreamClient *upstream.Client) error {
		loaded, readErr := upstreamClient.Client(callCtx, "manager-e2e-client")
		if readErr != nil {
			return readErr
		}
		clientView = loaded
		return nil
	}))
	require.True(t, clientView.Online)
	require.NotEmpty(t, clientView.Mappings)
	require.NotZero(t, clientView.Revision)

	status, body = managerClient.request(t, http.MethodGet,
		"/servers/"+serverView.ID+"/clients/manager-e2e-client", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), `data-base-revision="`+strconv.FormatInt(clientView.Revision, 10)+`"`)

	// Push a complete mapping list through the manager's mapping API.
	pushedMapping := config.Mapping{
		ID:         "manager-e2e-pushed",
		Type:       "tcp",
		RemotePort: 0,
		LocalAddr:  tcpEchoAddr,
		Note:       "pushed through manager",
		Enabled:    true,
	}
	localMapping := config.Mapping{
		ID:         "manager-e2e-local",
		Type:       "tcp",
		RemotePort: 0,
		LocalAddr:  tcpEchoAddr,
		Note:       "local exposure pushed through manager",
		Enabled:    true,
	}
	mappings := make([]config.Mapping, 0, len(clientView.Mappings)+2)
	for _, mapping := range clientView.Mappings {
		mappings = append(mappings, mapping.Mapping)
	}
	mappings = append(mappings, pushedMapping, localMapping)
	status, body = managerClient.request(t, http.MethodPut,
		"/api/servers/"+serverView.ID+"/clients/manager-e2e-client/mappings",
		map[string]any{
			"base_revision": clientView.Revision,
			"mappings":      mappings,
			"exposures":     map[string]view.Exposure{localMapping.ID: view.ExposureLocal},
		})
	require.Equal(t, http.StatusOK, status, string(body))
	var pushedView upstream.ClientView
	require.NoError(t, json.Unmarshal(body, &pushedView))
	require.Greater(t, pushedView.Revision, clientView.Revision)
	require.True(t, hasMapping(pushedView.Mappings, pushedMapping.ID))
	require.True(t, hasMapping(pushedView.Mappings, localMapping.ID))
	for _, mapping := range pushedView.Mappings {
		if mapping.ID == localMapping.ID {
			require.Equal(t, view.ExposureLocal, mapping.Exposure,
				"a new local mapping must use the server's safe exposures sequence")
		}
	}

	require.Eventually(t, func() bool {
		saved, loadErr := config.LoadClient(clientConfigPath)
		if loadErr != nil {
			return false
		}
		foundPushed := false
		foundLocal := false
		for _, mapping := range saved.Mappings {
			switch mapping.ID {
			case pushedMapping.ID:
				foundPushed = mapping.RemotePort != 0
			case localMapping.ID:
				foundLocal = true
			}
		}
		return foundPushed && foundLocal
	}, 5*time.Second, 20*time.Millisecond, "the real client must persist the manager-pushed mappings")

	// Refresh overview through the manager and verify the mapping count rose.
	status, body = managerClient.request(t, http.MethodPost, "/api/servers/"+serverView.ID+"/refresh", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var refreshed manager.ServerView
	require.NoError(t, json.Unmarshal(body, &refreshed))
	require.Equal(t, "online", refreshed.State)
	require.GreaterOrEqual(t, refreshed.MappingCount, 2)

	// Rotate the HY2 auth and control token while the real client is online.
	status, body = managerClient.request(t, http.MethodPut,
		"/api/servers/"+serverView.ID+"/settings/credentials", map[string]any{
			"auth":          e2eManagerNewAuth,
			"control_token": e2eManagerNewToken,
			"sync_mode":     "auto",
		})
	require.Equal(t, http.StatusOK, status, string(body))
	var rotation struct {
		Generation int64                       `json:"generation"`
		Results    []upstream.CredentialResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal(body, &rotation))
	require.Greater(t, rotation.Generation, int64(1))
	require.NotEmpty(t, rotation.Results)

	rotatedCfg, err := config.LoadServer(serverConfigPath)
	require.NoError(t, err)
	require.Equal(t, e2eManagerNewAuth, rotatedCfg.Auth.Password)
	require.Equal(t, e2eManagerNewToken, rotatedCfg.Control.Token)

	// Stop the real client and prove server-side exposure still works while
	// mapping edits are rejected as offline.
	closeClient()
	require.Eventually(t, func() bool {
		views := srv.Clients()
		return len(views) == 1 && !views[0].Online
	}, 10*time.Second, 20*time.Millisecond, "server must observe the client disconnect")

	var offlineView upstream.ClientView
	require.NoError(t, inventory.WithClient(context.Background(), serverView.ID, func(callCtx context.Context, upstreamClient *upstream.Client) error {
		loaded, readErr := upstreamClient.Client(callCtx, "manager-e2e-client")
		if readErr != nil {
			return readErr
		}
		offlineView = loaded
		return nil
	}))
	require.False(t, offlineView.Online)

	status, body = managerClient.request(t, http.MethodPut,
		"/api/servers/"+serverView.ID+"/clients/manager-e2e-client/mappings/"+pushedMapping.ID+"/exposure",
		map[string]any{"exposure": "local"})
	require.Equal(t, http.StatusOK, status, string(body))

	require.NoError(t, inventory.WithClient(context.Background(), serverView.ID, func(callCtx context.Context, upstreamClient *upstream.Client) error {
		loaded, readErr := upstreamClient.Client(callCtx, "manager-e2e-client")
		if readErr != nil {
			return readErr
		}
		for _, mapping := range loaded.Mappings {
			if mapping.ID == pushedMapping.ID {
				require.Equal(t, view.ExposureLocal, mapping.Exposure)
				return nil
			}
		}
		return &missingMappingError{id: pushedMapping.ID}
	}))

	status, body = managerClient.request(t, http.MethodPut,
		"/api/servers/"+serverView.ID+"/clients/manager-e2e-client/mappings",
		map[string]any{"base_revision": offlineView.Revision, "mappings": mappings})
	require.Equal(t, http.StatusConflict, status, string(body))
	require.Contains(t, string(body), `"code":"client_offline"`)

	status, body = managerClient.request(t, http.MethodGet,
		"/servers/"+serverView.ID+"/clients/manager-e2e-client", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), "离线客户端不可编辑")

	// Server settings work while the client is offline: mode, port policy,
	// and trust-proxy all go through the real server APIs.
	status, body = managerClient.request(t, http.MethodPut,
		"/api/servers/"+serverView.ID+"/settings/mode", map[string]any{"mode": "hy2"})
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = managerClient.request(t, http.MethodPut,
		"/api/servers/"+serverView.ID+"/settings/ports", map[string]any{
			"allow":                   []string{"31000-31999"},
			"deny":                    []string{},
			"auto_range":              "31000-31999",
			"offline_reserve_seconds": 60,
		})
	require.Equal(t, http.StatusOK, status, string(body))
	status, body = managerClient.request(t, http.MethodPut,
		"/api/servers/"+serverView.ID+"/settings/trust-proxy", map[string]any{"trust_proxy": true})
	require.Equal(t, http.StatusOK, status, string(body))

	status, body = managerClient.request(t, http.MethodGet,
		"/api/servers/"+serverView.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	var settings upstream.Settings
	require.NoError(t, json.Unmarshal(body, &settings))
	require.Equal(t, config.ModeHY2, settings.Mode)
	require.True(t, settings.TrustProxy)
	require.Equal(t, "31000-31999", settings.Ports.AutoRange)

	// Change the upstream Web administrator. The manager must store the new
	// password, re-authenticate, and keep working.
	status, body = managerClient.request(t, http.MethodPut,
		"/api/servers/"+serverView.ID+"/settings/web", map[string]any{
			"username":         "admin",
			"new_password":     e2eManagerNewWebPassword,
			"confirm_password": e2eManagerNewWebPassword,
		})
	require.Equal(t, http.StatusOK, status, string(body))

	status, body = managerClient.request(t, http.MethodGet,
		"/api/servers/"+serverView.ID+"/settings", nil)
	require.Equal(t, http.StatusOK, status, string(body))
	storedPassword, err := managerStore.ServerPassword(serverView.ID)
	require.NoError(t, err)
	require.Equal(t, e2eManagerNewWebPassword, string(storedPassword),
		"the manager must store the upstream password accepted by the server")

	serverWebClient := &http.Client{Timeout: 10 * time.Second}
	loginResp, err := serverWebClient.PostForm("http://"+webAddr+"/api/login", url.Values{
		"username": {"admin"},
		"password": {e2eManagerNewWebPassword},
	})
	require.NoError(t, err)
	require.NoError(t, loginResp.Body.Close())
	require.Equal(t, http.StatusNoContent, loginResp.StatusCode,
		"the real server must accept the Web password stored by the manager")

	// The manager audit log must contain the actions but none of the
	// plaintext credentials.
	auditData, err := os.ReadFile(auditPath)
	require.NoError(t, err)
	auditText := string(auditData)
	require.Contains(t, auditText, "mapping.update")
	require.Contains(t, auditText, "settings.credentials.rotate")
	require.Contains(t, auditText, "settings.web")
	for _, secret := range []string{
		e2eWebPassword,
		e2eManagerNewWebPassword,
		e2ePassword,
		e2eManagerNewAuth,
		e2eManagerNewToken,
	} {
		require.NotContains(t, auditText, secret)
	}
}

type missingMappingError struct{ id string }

func (e *missingMappingError) Error() string { return "mapping not found: " + e.id }

func hasMapping(mappings []upstream.MappingView, id string) bool {
	for _, mapping := range mappings {
		if mapping.ID == id {
			return true
		}
	}
	return false
}

// startManagerForE2E builds a manager store with a known administrator and
// serves the production Web handler through httptest. The manager still talks
// to the real hy2tund over its production upstream HTTP adapter.
func startManagerForE2E(t *testing.T, parent string, logger *slog.Logger) (string, *manager.Inventory, *manager.Store, string) {
	t.Helper()

	dir := filepath.Join(parent, "manager")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	statePath := filepath.Join(dir, "state.json")
	keyPath := filepath.Join(dir, "manager.key")
	auditPath := filepath.Join(dir, "audit.jsonl")

	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyPath, []byte(hex.EncodeToString(key)+"\n"), 0o600))
	hash, err := bcrypt.GenerateFromPassword([]byte(e2eManagerAdminPassword), bcrypt.MinCost)
	require.NoError(t, err)
	state := manager.State{
		Admin:   manager.AdminRecord{Username: "admin", PasswordHash: string(hash)},
		Servers: []manager.ServerRecord{},
	}
	encoded, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, append(encoded, '\n'), 0o600))

	store, err := manager.OpenStore(statePath, keyPath)
	require.NoError(t, err)
	audit := manager.NewAuditLog(auditPath)
	inventory := manager.NewInventory(store, audit, nil, logger)
	web, err := manager.NewWebServer(store, audit, manager.Config{
		BasePath:        "/",
		StateFile:       statePath,
		KeyFile:         keyPath,
		AuditFile:       auditPath,
		RefreshInterval: time.Second,
	}, logger)
	require.NoError(t, err)
	web.SetInventory(inventory)
	ts := httptest.NewServer(web.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, inventory, store, auditPath
}

type managerE2EClient struct {
	t      *testing.T
	base   string
	client *http.Client
	cookie *http.Cookie
}

func newManagerE2EClient(t *testing.T, base string) *managerE2EClient {
	t.Helper()

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(base+"/api/login", url.Values{
		"username": {"admin"},
		"password": {e2eManagerAdminPassword},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	cookies := resp.Cookies()
	require.NotEmpty(t, cookies)
	return &managerE2EClient{t: t, base: base, client: client, cookie: cookies[0]}
}

func (c *managerE2EClient) request(t *testing.T, method, path string, payload any) (int, []byte) {
	t.Helper()

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		require.NoError(t, err)
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	require.NoError(t, err)
	req.AddCookie(c.cookie)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("X-Requested-With", "hy2tun")
		req.Header.Set("Origin", c.base)
	}
	resp, err := c.client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, data
}
