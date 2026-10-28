package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/client"
	"hy2tun/internal/config"
	"hy2tun/internal/testutil"
)

// TestCredentialRotationEndToEnd proves the complete auto-push path with a real
// server and client: rotate, persist on the client, reconnect, keep the reverse
// TCP mapping alive, then revoke the previous pair and reject an old client.
func TestCredentialRotationEndToEnd(t *testing.T) {
	echoAddr, stopEcho := testutil.StartTCPEcho(t)
	defer stopEcho()

	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1 &&
			v[0].Mappings[0].Status == "running" &&
			v[0].Mappings[0].AssignedPort != 0
	}))
	publicAddr := fmt.Sprintf("127.0.0.1:%d", pair.Server.Clients()[0].Mappings[0].AssignedPort)
	requirePublicTCPEcho(t, publicAddr, "before-rotation")

	const (
		newAuth  = "rotated-auth-password"
		newToken = "fedcba9876543210fedcba9876543210"
	)
	generation, results, err := pair.Server.RotateCredentials(newAuth, newToken, "auto")
	require.NoError(t, err)
	require.Equal(t, int64(2), generation)
	require.Len(t, results, 1)
	require.Equal(t, "synced", results[0].Status)

	// The client must persist the pushed pair and reconnect with it.
	require.NoError(t, waitFor(t, 10*time.Second, func() bool {
		saved, err := config.LoadClient(pair.ClientConfigPath)
		if err != nil {
			return false
		}
		return saved.Credentials.Generation == generation &&
			saved.Auth == newAuth &&
			saved.Control.Token == newToken
	}))

	// The server view must reflect the reconnected client on the new
	// generation, and the mapping must be re-registered on its old port.
	require.NoError(t, waitFor(t, 10*time.Second, func() bool {
		v := pair.Server.Clients()
		if len(v) != 1 {
			return false
		}
		c := v[0]
		if !c.Online || c.CredentialGeneration != generation || !c.CredentialSynced {
			return false
		}
		return len(c.Mappings) == 1 &&
			c.Mappings[0].Status == "running" &&
			fmt.Sprintf("127.0.0.1:%d", c.Mappings[0].AssignedPort) == publicAddr
	}))

	// The reverse TCP mapping must still carry traffic after the reconnect.
	require.Eventually(t, func() bool {
		return publicTCPEcho(publicAddr, "after-rotation") == nil
	}, 5*time.Second, 50*time.Millisecond, "reverse TCP mapping must survive credential rotation")
	requirePublicTCPEcho(t, publicAddr, "after-rotation")

	require.NoError(t, pair.Server.RevokePreviousCredentials())
	snapshot := pair.Server.CredentialsSnapshot()
	require.Equal(t, int64(0), snapshot.PreviousGeneration)
	require.False(t, snapshot.AcceptAuth(defaultTestAuth))
	_, ok := snapshot.AcceptControlToken(defaultTestToken)
	require.False(t, ok)

	// Revoking the previous pair must not disturb the client that already
	// reconnected on the current generation.
	require.Eventually(t, func() bool {
		return publicTCPEcho(publicAddr, "after-revoke") == nil
	}, 5*time.Second, 50*time.Millisecond,
		"the current client must keep serving after the previous pair is revoked")
	requirePublicTCPEcho(t, publicAddr, "after-revoke")
	currentViews := pair.Server.Clients()
	require.Len(t, currentViews, 1)
	require.True(t, currentViews[0].Online)
	require.Equal(t, generation, currentViews[0].CredentialGeneration)
	require.True(t, currentViews[0].CredentialSynced)

	// A second client that still holds the revoked pair must never register.
	oldCfg := testClientConfig(pair.Server.ListenAddr().String(), nil)
	oldCfg.Client.Name = "revoked-client"
	oldClient, err := client.New(oldCfg, slog.Default())
	require.NoError(t, err)
	oldCtx, stopOld := context.WithCancel(context.Background())
	defer stopOld()
	go func() { _ = oldClient.Start(oldCtx) }()
	defer oldClient.Close()

	readyCtx, cancelReady := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelReady()
	require.Error(t, oldClient.WaitReady(readyCtx),
		"a client with revoked credentials must not complete the control handshake")

	for _, v := range pair.Server.Clients() {
		require.NotEqual(t, oldCfg.Client.ID, v.ID,
			"the revoked client must not appear in the server view")
	}
}

// publicTCPEcho performs one echo round trip through a public mapping port.
func publicTCPEcho(addr, message string) error {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	requireDeadline := time.Now().Add(2 * time.Second)
	if err := conn.SetDeadline(requireDeadline); err != nil {
		return err
	}
	if _, err := conn.Write([]byte(message)); err != nil {
		return err
	}
	buf := make([]byte, len(message))
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if string(buf) != message {
		return fmt.Errorf("tcp echo: got %q, want %q", buf, message)
	}
	return nil
}

// requirePublicTCPEcho asserts that a public mapping port echoes a message.
func requirePublicTCPEcho(t *testing.T, addr, message string) {
	t.Helper()
	require.NoError(t, publicTCPEcho(addr, message))
}

const (
	integrationServerWebPassword = "server-web-password"
	integrationClientWebPassword = "client-web-password"
)

// webIntegrationPair is a running server and client that both expose their web
// consoles. It is used to prove that hot settings work through the real HTTP
// handlers rather than a fake API.
type webIntegrationPair struct {
	Server           *Server
	Client           *client.Client
	ClientConfigPath string
	ServerConfigPath string
	ServerWebAddr    string
	ClientWebAddr    string
	Stop             func()
}

// startWebIntegrationPair starts a server and client with web consoles enabled
// on loopback ephemeral ports. The server allows ports 25000-25099 and persists
// its configuration so hot updates can be checked on disk.
func startWebIntegrationPair(t *testing.T, mappings []config.Mapping) *webIntegrationPair {
	t.Helper()

	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	dir := t.TempDir()

	serverCfg := validServerConfig(cert)
	serverCfg.ConfigPath = filepath.Join(dir, "server.yaml")
	serverCfg.Ports = config.PortsConfig{
		Allow:     []string{"25000-25099"},
		AutoRange: "25000-25099",
	}
	serverHash, err := bcrypt.GenerateFromPassword([]byte(integrationServerWebPassword), bcrypt.MinCost)
	require.NoError(t, err)
	serverCfg.Web = config.WebConfig{
		Listen:       "127.0.0.1:0",
		Username:     "admin",
		PasswordHash: string(serverHash),
	}
	require.NoError(t, config.SaveServer(serverCfg.ConfigPath, serverCfg))

	srv, stopServer := startServer(t, serverCfg)

	clientCfg := testClientConfig(srv.ListenAddr().String(), mappings)
	clientHash, err := bcrypt.GenerateFromPassword([]byte(integrationClientWebPassword), bcrypt.MinCost)
	require.NoError(t, err)
	clientCfg.Web = config.WebConfig{
		Listen:       "127.0.0.1:0",
		Username:     "admin",
		PasswordHash: string(clientHash),
	}
	clientConfigPath := filepath.Join(dir, "client.yaml")
	require.NoError(t, config.SaveClient(clientConfigPath, clientCfg))

	cl, err := client.New(clientCfg, slog.Default())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = cl.Start(ctx) }()

	readyCtx, cancelReady := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelReady()
	require.NoError(t, cl.WaitReady(readyCtx))

	serverWebAddr := waitForWebAt(t, srv.WebAddr)
	clientWebAddr := waitForWebAt(t, cl.WebAddr)

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			_ = cl.Close()
			stopServer()
		})
	}
	t.Cleanup(stop)

	return &webIntegrationPair{
		Server:           srv,
		Client:           cl,
		ClientConfigPath: clientConfigPath,
		ServerConfigPath: serverCfg.ConfigPath,
		ServerWebAddr:    serverWebAddr,
		ClientWebAddr:    clientWebAddr,
		Stop:             stop,
	}
}

// waitForWebAt waits until addr resolves to a bound listener that answers the
// login page.
func waitForWebAt(t *testing.T, addr func() string) string {
	t.Helper()

	client := &http.Client{Timeout: 2 * time.Second}
	var webAddr string
	require.Eventually(t, func() bool {
		webAddr = addr()
		if webAddr == "" || webAddr == "127.0.0.1:0" {
			return false
		}
		resp, err := client.Get("http://" + webAddr + "/login")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond, "web console must become reachable")
	return webAddr
}

// loginWebAt authenticates against one web console and returns its session
// cookie.
func loginWebAt(t *testing.T, client *http.Client, webAddr, username, password string) *http.Cookie {
	t.Helper()

	resp, err := client.PostForm("http://"+webAddr+"/api/login", url.Values{
		"username": {username},
		"password": {password},
	})
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	cookies := resp.Cookies()
	require.NotEmpty(t, cookies)
	return cookies[0]
}

// doWebJSON sends an authenticated, CSRF-protected JSON request to a web
// console. A nil payload sends no body.
func doWebJSON(t *testing.T, client *http.Client, method, webAddr, path string, cookie *http.Cookie, payload any) *http.Response {
	t.Helper()

	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		require.NoError(t, err)
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, "http://"+webAddr+path, body)
	require.NoError(t, err)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Requested-With", "hy2tun")
	req.Header.Set("Origin", "http://"+webAddr)
	req.AddCookie(cookie)

	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp
}

// readWebBody reads and closes a response body.
func readWebBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(data)
}

// TestManualRotationViaClientWebEndToEnd proves the manual path: the server
// rotates without pushing, the client keeps its old config until an operator
// submits the new generation through the client web API, then reconnects and
// reports synced. It also proves client rename propagation and secret reveal.
func TestManualRotationViaClientWebEndToEnd(t *testing.T) {
	echoAddr, stopEcho := testutil.StartTCPEcho(t)
	defer stopEcho()

	pair := startWebIntegrationPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1 &&
			v[0].Mappings[0].Status == "running" &&
			v[0].Mappings[0].AssignedPort != 0
	}))
	publicAddr := fmt.Sprintf("127.0.0.1:%d", pair.Server.Clients()[0].Mappings[0].AssignedPort)
	requirePublicTCPEcho(t, publicAddr, "before-manual-rotation")

	const (
		newAuth  = "manual-rotated-password"
		newToken = "abcdef0123456789abcdef0123456789"
	)
	generation, results, err := pair.Server.RotateCredentials(newAuth, newToken, "manual")
	require.NoError(t, err)
	require.Equal(t, int64(2), generation)
	require.Len(t, results, 1)
	require.Equal(t, "pending", results[0].Status)

	// Manual rotation must not touch the client configuration.
	saved, err := config.LoadClient(pair.ClientConfigPath)
	require.NoError(t, err)
	require.Equal(t, int64(1), saved.Credentials.Generation)
	require.Equal(t, defaultTestAuth, saved.Auth)
	require.Equal(t, defaultTestToken, saved.Control.Token)

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && v[0].CredentialGeneration == 1 && !v[0].CredentialSynced
	}))

	webClient := &http.Client{Timeout: 10 * time.Second}
	cookie := loginWebAt(t, webClient, pair.ClientWebAddr, "admin", integrationClientWebPassword)

	// The client still reports its local generation until the manual sync.
	resp := doWebJSON(t, webClient, http.MethodGet, pair.ClientWebAddr, "/api/settings", cookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var settings struct {
		Credentials struct {
			Generation       int64 `json:"generation"`
			ServerGeneration int64 `json:"server_generation"`
			Synced           bool  `json:"synced"`
		} `json:"credentials"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&settings))
	resp.Body.Close()
	// The client cannot learn about a manual server rotation until it
	// reconnects, so its local generation and last-known server generation
	// stay unchanged.
	require.Equal(t, int64(1), settings.Credentials.Generation)
	require.Equal(t, int64(1), settings.Credentials.ServerGeneration)

	// Submit the new generation through the real client web API.
	resp = doWebJSON(t, webClient, http.MethodPut, pair.ClientWebAddr, "/api/settings/credentials", cookie, map[string]any{
		"current_password": integrationClientWebPassword,
		"generation":       generation,
		"auth":             newAuth,
		"control_token":    newToken,
	})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	var accepted struct {
		Generation int64  `json:"generation"`
		Status     string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&accepted))
	resp.Body.Close()
	require.Equal(t, generation, accepted.Generation)
	require.Equal(t, "pending", accepted.Status)

	require.NoError(t, waitFor(t, 10*time.Second, func() bool {
		updated, err := config.LoadClient(pair.ClientConfigPath)
		if err != nil {
			return false
		}
		return updated.Credentials.Generation == generation &&
			updated.Auth == newAuth &&
			updated.Control.Token == newToken
	}))

	require.NoError(t, waitFor(t, 10*time.Second, func() bool {
		v := pair.Server.Clients()
		if len(v) != 1 {
			return false
		}
		c := v[0]
		if !c.Online || c.CredentialGeneration != generation || !c.CredentialSynced {
			return false
		}
		return len(c.Mappings) == 1 &&
			c.Mappings[0].Status == "running" &&
			fmt.Sprintf("127.0.0.1:%d", c.Mappings[0].AssignedPort) == publicAddr
	}))

	require.Eventually(t, func() bool {
		return publicTCPEcho(publicAddr, "after-manual-rotation") == nil
	}, 5*time.Second, 50*time.Millisecond, "reverse TCP mapping must survive manual rotation")
	requirePublicTCPEcho(t, publicAddr, "after-manual-rotation")

	// Rename the client through its web console; the server view must follow.
	resp = doWebJSON(t, webClient, http.MethodPut, pair.ClientWebAddr, "/api/settings/name", cookie, map[string]any{
		"current_password": integrationClientWebPassword,
		"name":             "renamed-through-web",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && v[0].Name == "renamed-through-web"
	}))
	updated, err := config.LoadClient(pair.ClientConfigPath)
	require.NoError(t, err)
	require.Equal(t, "renamed-through-web", updated.Client.Name)

	// Reveal is re-authenticated, never cached, and returns the new pair.
	resp = doWebJSON(t, webClient, http.MethodPost, pair.ClientWebAddr, "/api/settings/credentials/reveal", cookie, map[string]any{
		"password": "wrong-password",
	})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.NotContains(t, readWebBody(t, resp), newAuth)

	resp = doWebJSON(t, webClient, http.MethodPost, pair.ClientWebAddr, "/api/settings/credentials/reveal", cookie, map[string]any{
		"password": integrationClientWebPassword,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var revealed struct {
		Current struct {
			Generation   int64  `json:"generation"`
			Auth         string `json:"auth"`
			ControlToken string `json:"control_token"`
		} `json:"current"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&revealed))
	resp.Body.Close()
	require.Equal(t, generation, revealed.Current.Generation)
	require.Equal(t, newAuth, revealed.Current.Auth)
	require.Equal(t, newToken, revealed.Current.ControlToken)
}

// TestServerWebHotSettingsEndToEnd proves that the server web API can reveal
// secrets, change the port allow list, reject a newly disallowed mapping while
// keeping the existing one alive, and replace the web administrator.
func TestServerWebHotSettingsEndToEnd(t *testing.T) {
	echoAddr, stopEcho := testutil.StartTCPEcho(t)
	defer stopEcho()

	pair := startWebIntegrationPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1 &&
			v[0].Mappings[0].Status == "running" &&
			v[0].Mappings[0].AssignedPort != 0 &&
			v[0].Revision > 0
	}))
	current := pair.Server.Clients()[0]
	publicAddr := fmt.Sprintf("127.0.0.1:%d", current.Mappings[0].AssignedPort)
	publicPort := current.Mappings[0].AssignedPort
	requirePublicTCPEcho(t, publicAddr, "before-hot-settings")

	webClient := &http.Client{Timeout: 10 * time.Second}
	cookie := loginWebAt(t, webClient, pair.ServerWebAddr, "admin", integrationServerWebPassword)

	// Reveal: a wrong password is rejected, the correct one returns the live
	// pair with an explicit no-store policy.
	resp := doWebJSON(t, webClient, http.MethodPost, pair.ServerWebAddr, "/api/settings/credentials/reveal", cookie, map[string]any{
		"password": "wrong-password",
	})
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.NotContains(t, readWebBody(t, resp), defaultTestAuth)

	resp = doWebJSON(t, webClient, http.MethodPost, pair.ServerWebAddr, "/api/settings/credentials/reveal", cookie, map[string]any{
		"password": integrationServerWebPassword,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	var revealed struct {
		Current struct {
			Generation   int64  `json:"generation"`
			Auth         string `json:"auth"`
			ControlToken string `json:"control_token"`
		} `json:"current"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&revealed))
	resp.Body.Close()
	require.Equal(t, int64(1), revealed.Current.Generation)
	require.Equal(t, defaultTestAuth, revealed.Current.Auth)
	require.Equal(t, defaultTestToken, revealed.Current.ControlToken)

	// Hot port policy: only the existing mapping's port stays allowed.
	resp = doWebJSON(t, webClient, http.MethodPut, pair.ServerWebAddr, "/api/settings/ports", cookie, map[string]any{
		"current_password":        integrationServerWebPassword,
		"allow":                   []string{strconv.Itoa(publicPort)},
		"deny":                    []string{},
		"auto_range":              "25000-25099",
		"offline_reserve_seconds": 3600,
	})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	views := pair.Server.Clients()
	require.Len(t, views, 1)
	require.Equal(t, "running", views[0].Mappings[0].Status)
	requirePublicTCPEcho(t, publicAddr, "existing-mapping-survives-policy-change")

	// A new mapping on a port outside the new allow list is rejected.
	disallowedPort := 25099
	if disallowedPort == publicPort {
		disallowedPort = 25098
	}
	pushBody := map[string]any{
		"base_revision": views[0].Revision,
		"mappings": []config.Mapping{
			{ID: "blocked", Type: "tcp", RemotePort: disallowedPort, LocalAddr: echoAddr, Enabled: true},
		},
	}
	resp = doWebJSON(t, webClient, http.MethodPut, pair.ServerWebAddr,
		"/api/clients/"+url.PathEscape(views[0].ID)+"/mappings", cookie, pushBody)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
	var pushErr struct {
		Code    string `json:"code"`
		Results []struct {
			ID    string `json:"id"`
			Error string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&pushErr))
	resp.Body.Close()
	require.Equal(t, "port_not_allowed", pushErr.Code)
	require.Len(t, pushErr.Results, 1)
	require.Equal(t, "port_not_allowed", pushErr.Results[0].Error)

	// The failed push must not disturb the mapping that was already running.
	requirePublicTCPEcho(t, publicAddr, "existing-mapping-survives-rejected-push")

	// Replace the web administrator. The old session and old password must
	// stop working immediately; the new pair must log in.
	resp = doWebJSON(t, webClient, http.MethodPut, pair.ServerWebAddr, "/api/settings/web", cookie, map[string]any{
		"current_password": integrationServerWebPassword,
		"username":         "root",
		"new_password":     "new-server-web-password",
		"confirm_password": "new-server-web-password",
	})
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()

	resp = doWebJSON(t, webClient, http.MethodGet, pair.ServerWebAddr, "/api/settings", cookie, nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	oldLogin, err := webClient.PostForm("http://"+pair.ServerWebAddr+"/api/login", url.Values{
		"username": {"admin"},
		"password": {integrationServerWebPassword},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, oldLogin.StatusCode)
	oldLogin.Body.Close()

	newCookie := loginWebAt(t, webClient, pair.ServerWebAddr, "root", "new-server-web-password")
	resp = doWebJSON(t, webClient, http.MethodGet, pair.ServerWebAddr, "/api/settings", newCookie, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	persisted, err := config.LoadServer(pair.ServerConfigPath)
	require.NoError(t, err)
	require.Equal(t, "root", persisted.Web.Username)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(persisted.Web.PasswordHash), []byte("new-server-web-password")))

	// The credential generation is unaffected by the web admin change.
	require.Equal(t, int64(1), pair.Server.CredentialsSnapshot().Generation)
	require.Equal(t, int64(0), pair.Server.CredentialsSnapshot().PreviousGeneration)
}

// TestCredentialRotationKeepsTCPAndUDP covers the reconnect race with both
// protocols. A rotated client reconnects and asks for the same explicit ports;
// the server must have released the old session's TCP and UDP listeners by
// then.
func TestCredentialRotationKeepsTCPAndUDP(t *testing.T) {
	tcpEchoAddr, stopTCP := testutil.StartTCPEcho(t)
	defer stopTCP()
	udpEchoAddr, stopUDP := testutil.StartUDPEcho(t)
	defer stopUDP()

	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: tcpEchoAddr, Enabled: true},
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: udpEchoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		if len(v) != 1 || len(v[0].Mappings) != 2 {
			return false
		}
		for _, mapping := range v[0].Mappings {
			if mapping.Status != "running" || mapping.AssignedPort == 0 {
				return false
			}
		}
		return true
	}))

	_, _, err := pair.Server.RotateCredentials("rotated-auth-password", "fedcba9876543210fedcba9876543210", "auto")
	require.NoError(t, err)

	require.NoError(t, waitFor(t, 10*time.Second, func() bool {
		v := pair.Server.Clients()
		if len(v) != 1 || v[0].CredentialGeneration != 2 || !v[0].CredentialSynced {
			return false
		}
		if len(v[0].Mappings) != 2 {
			return false
		}
		for _, mapping := range v[0].Mappings {
			if mapping.Status != "running" {
				return false
			}
		}
		return true
	}), "both mappings must be running after the reconnect: %+v", pair.Server.Clients())
}

// TestHotConfigWriteFailureLeavesRuntimeUnchanged proves that every hot
// settings mutation persists the configuration before swapping runtime state.
// A failed write must leave credentials, ports, the web administrator, and the
// live client untouched.
func TestHotConfigWriteFailureLeavesRuntimeUnchanged(t *testing.T) {
	echoAddr, stopEcho := testutil.StartTCPEcho(t)
	defer stopEcho()

	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	serverCfg := validServerConfig(cert)
	dir := t.TempDir()
	serverCfg.ConfigPath = filepath.Join(dir, "server.yaml")
	require.NoError(t, config.SaveServer(serverCfg.ConfigPath, serverCfg))

	srv, stopServer := startServer(t, serverCfg)
	defer stopServer()

	clientCfg := testClientConfig(srv.ListenAddr().String(), []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	clientConfigPath := filepath.Join(dir, "client.yaml")
	require.NoError(t, config.SaveClient(clientConfigPath, clientCfg))
	cl, err := client.New(clientCfg, slog.Default())
	require.NoError(t, err)
	clientCtx, stopClient := context.WithCancel(context.Background())
	defer stopClient()
	go func() { _ = cl.Start(clientCtx) }()
	defer func() { _ = cl.Close() }()

	readyCtx, cancelReady := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelReady()
	require.NoError(t, cl.WaitReady(readyCtx))

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := srv.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1 &&
			v[0].Mappings[0].Status == "running" &&
			v[0].Mappings[0].AssignedPort != 0
	}))
	publicAddr := fmt.Sprintf("127.0.0.1:%d", srv.Clients()[0].Mappings[0].AssignedPort)
	requirePublicTCPEcho(t, publicAddr, "before-write-failure")

	beforeCreds := srv.CredentialsSnapshot()
	beforeSummary := srv.ConfigSummary()

	writeErr := errors.New("write failed")
	originalSave := srv.saveConfig
	srv.saveConfig = func(string, *config.ServerConfig) error { return writeErr }
	// Restore the seam before the test cleanup stops the server.
	defer func() { srv.saveConfig = originalSave }()

	// RotateCredentials persists the new generation before swapping runtime
	// credentials. A failed write must not change the live pair or push to the
	// connected client.
	_, _, err = srv.RotateCredentials("new-auth-password", "new-control-token-0123456789", "auto")
	require.ErrorIs(t, err, writeErr)
	afterCreds := srv.CredentialsSnapshot()
	require.Equal(t, beforeCreds.Generation, afterCreds.Generation)
	require.Equal(t, beforeCreds.Auth, afterCreds.Auth)
	require.Equal(t, beforeCreds.ControlToken, afterCreds.ControlToken)
	require.Equal(t, beforeCreds.PreviousGeneration, afterCreds.PreviousGeneration)
	require.Equal(t, beforeCreds.PreviousAuth, afterCreds.PreviousAuth)
	require.Equal(t, beforeCreds.PreviousControlToken, afterCreds.PreviousControlToken)
	requirePublicTCPEcho(t, publicAddr, "after-rotate-write-failure")

	// UpdatePorts persists before swapping the live policy.
	err = srv.UpdatePorts([]string{"26000"}, nil, "26000-26099", 2*time.Hour)
	require.ErrorIs(t, err, writeErr)
	afterSummary := srv.ConfigSummary()
	require.Equal(t, beforeSummary.PortsAllow, afterSummary.PortsAllow)
	require.Equal(t, beforeSummary.PortsDeny, afterSummary.PortsDeny)
	require.Equal(t, beforeSummary.PortsAutoRange, afterSummary.PortsAutoRange)
	require.Equal(t, beforeSummary.OfflineReserve, afterSummary.OfflineReserve)
	requirePublicTCPEcho(t, publicAddr, "after-ports-write-failure")

	// UpdateWebConfig persists before replacing the live web administrator.
	hash, err := bcrypt.GenerateFromPassword([]byte("new-web-password"), bcrypt.MinCost)
	require.NoError(t, err)
	err = srv.UpdateWebConfig("root", string(hash))
	require.ErrorIs(t, err, writeErr)
	afterSummary = srv.ConfigSummary()
	require.Equal(t, beforeSummary.WebUsername, afterSummary.WebUsername)
	require.Equal(t, beforeSummary.WebPasswordConfigured, afterSummary.WebPasswordConfigured)
	requirePublicTCPEcho(t, publicAddr, "after-web-write-failure")

	// The client was never pushed or disconnected by the failed mutations.
	views := srv.Clients()
	require.Len(t, views, 1)
	require.True(t, views[0].Online)
	require.Equal(t, beforeCreds.Generation, views[0].CredentialGeneration)
	require.True(t, views[0].CredentialSynced)
	require.Len(t, views[0].Mappings, 1)
	require.Equal(t, "running", views[0].Mappings[0].Status)
}
