package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/client"
	"hy2tun/internal/config"
	"hy2tun/internal/server"
	"hy2tun/internal/testutil"
	"hy2tun/internal/view"
)

const exposureTestClientID = "exposure-e2e"

// TestMappingExposureE2E starts a real server and client, verifies that a
// public mapping is reachable on the available address families, switches the
// mapping to local through the real HTTP API, and checks the closed public
// connection, refused public dials, working loopback listeners, client
// read-only view, and the switch back to public.
func TestMappingExposureE2E(t *testing.T) {
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)
	udpEchoAddr, _ := testutil.StartUDPEcho(t)

	dir := t.TempDir()
	cfg := newExposureServerConfig(t, dir, func(c *config.ServerConfig) {
		hash, err := bcryptHash(e2eWebPassword)
		require.NoError(t, err)
		c.Web = config.WebConfig{Listen: "127.0.0.1:0", Username: "admin", PasswordHash: hash}
	})
	port := freePortForBoth(t, cfg.Ports.Allow[0])
	cfg.Ports.Allow = []string{strconv.Itoa(port)}
	cfg.Ports.AutoRange = fmt.Sprintf("%d-%d", port, port)
	h := startExposureServer(t, cfg)
	webAddr := waitForWeb(t, h.srv)
	webClient := &http.Client{Timeout: 10 * time.Second}
	cookie := loginWeb(t, webClient, webAddr)

	clientCfg := newE2EClientConfig(t, t.TempDir(), exposureTestClientID,
		h.srv.ListenAddr().String(), h.cfg.TLS.Cert, config.ModeHY2, tcpEchoAddr, udpEchoAddr)
	clientCfg.Mappings[0].RemotePort = port
	clientCfg.Mappings[1].RemotePort = port
	ec := startE2EClient(t, clientCfg, testFailoverOptions())
	ec.waitReady(t, 10*time.Second)

	tcpView := waitServerMapping(t, h.srv, exposureTestClientID, "tcp", 10*time.Second,
		func(m view.MappingView) bool { return m.Status == "running" && m.AssignedPort > 0 })
	udpView := waitServerMapping(t, h.srv, exposureTestClientID, "udp", 10*time.Second,
		func(m view.MappingView) bool { return m.Status == "running" && m.AssignedPort > 0 })
	require.Equal(t, view.ExposurePublic, tcpView.Exposure, "new mappings default to public")
	require.Equal(t, view.ExposurePublic, udpView.Exposure)
	require.Equal(t, port, tcpView.AssignedPort)
	require.Equal(t, port, udpView.AssignedPort)
	tcpPort, udpPort := tcpView.AssignedPort, udpView.AssignedPort
	require.True(t, !containsFamily(tcpView.UnavailableFamilies, "ipv4") ||
		!containsFamily(tcpView.UnavailableFamilies, "ipv6"), "at least one family must bind")

	public4 := firstNonLoopbackIP(t, false)
	if public4 == "" {
		t.Skipf("TestMappingExposureE2E requires a non-loopback IPv4 address; none is available on this host")
	}
	publicTCP4 := net.JoinHostPort(public4, strconv.Itoa(tcpPort))
	publicUDP4 := net.JoinHostPort(public4, strconv.Itoa(udpPort))

	require.Eventually(t, func() bool { return tcpEcho(publicTCP4, "public-v4") == nil },
		5*time.Second, 20*time.Millisecond, "public TCP mapping must be reachable on IPv4")
	require.Eventually(t, func() bool { return udpEcho(publicUDP4, "public-udp-v4") == nil },
		5*time.Second, 20*time.Millisecond, "public UDP mapping must be reachable on IPv4")
	require.NoError(t, tcpEcho(loopbackTCP(tcpPort), "public-loopback-v4"))
	require.NoError(t, udpEcho(loopbackUDP(udpPort), "public-loopback-udp-v4"))
	if public6 := firstNonLoopbackIP(t, true); public6 != "" &&
		!containsFamily(tcpView.UnavailableFamilies, "ipv6") {
		require.Eventually(t, func() bool {
			return tcpEcho(net.JoinHostPort(public6, strconv.Itoa(tcpPort)), "public-v6") == nil
		}, 5*time.Second, 20*time.Millisecond, "public TCP mapping must be reachable on IPv6")
		require.Eventually(t, func() bool {
			return udpEcho(net.JoinHostPort(public6, strconv.Itoa(udpPort)), "public-udp-v6") == nil
		}, 5*time.Second, 20*time.Millisecond, "public UDP mapping must be reachable on IPv6")
	} else if containsFamily(tcpView.UnavailableFamilies, "ipv6") {
		t.Logf("IPv6 is unavailable in the public bind (%v); public IPv6 reachability not asserted",
			tcpView.UnavailableFamilies)
	} else {
		t.Logf("no reachable non-loopback IPv6 address; public IPv6 reachability not asserted")
	}

	waitClientAssignedPort(t, ec.path, "tcp", tcpPort, 5*time.Second)
	waitClientAssignedPort(t, ec.path, "udp", udpPort, 5*time.Second)
	beforeRevision := waitClientRevisionStable(t, ec.path, 5*time.Second)
	rawBefore, err := os.ReadFile(ec.path)
	require.NoError(t, err)
	require.NotContains(t, string(rawBefore), "exposure:", "client YAML must not contain exposure state")

	persistent, err := net.DialTimeout("tcp", publicTCP4, 2*time.Second)
	require.NoError(t, err)
	defer persistent.Close()
	requireTCPEcho(t, persistent, "before-local-switch")

	localTCP := setExposureWeb(t, webClient, webAddr, cookie, exposureTestClientID, "tcp", "local")
	require.Equal(t, view.ExposureLocal, localTCP.Exposure)
	localUDP := setExposureWeb(t, webClient, webAddr, cookie, exposureTestClientID, "udp", "local")
	require.Equal(t, view.ExposureLocal, localUDP.Exposure)

	requireConnClosed(t, persistent, 5*time.Second)
	waitServerMapping(t, h.srv, exposureTestClientID, "tcp", 5*time.Second,
		func(m view.MappingView) bool { return m.Status == "running" && m.Exposure == view.ExposureLocal })
	waitServerMapping(t, h.srv, exposureTestClientID, "udp", 5*time.Second,
		func(m view.MappingView) bool { return m.Status == "running" && m.Exposure == view.ExposureLocal })
	waitClientMapping(t, ec.cl, "tcp", 5*time.Second,
		func(m view.MappingView) bool { return m.Exposure == view.ExposureLocal })
	waitClientMapping(t, ec.cl, "udp", 5*time.Second,
		func(m view.MappingView) bool { return m.Exposure == view.ExposureLocal })

	require.Equal(t, beforeRevision, waitClientRevisionStable(t, ec.path, 5*time.Second),
		"a server exposure update must not change the client revision")
	rawAfter, err := os.ReadFile(ec.path)
	require.NoError(t, err)
	require.NotContains(t, string(rawAfter), "exposure:", "client YAML must stay read-only for exposure")

	requireTCPRefused(t, publicTCP4)
	requireUDPNoEcho(t, publicUDP4)
	require.NoError(t, tcpEcho(loopbackTCP(tcpPort), "local-loopback-v4"))
	require.Eventually(t, func() bool { return udpEcho(loopbackUDP(udpPort), "local-loopback-udp-v4") == nil },
		10*time.Second, 50*time.Millisecond, "local UDP mapping must recover after the switch")

	if err := ipv6LoopbackUnavailable(); err != nil {
		require.Contains(t, tcpView.UnavailableFamilies, "ipv6",
			"IPv6 is unavailable on this host, so the public bind must report it")
		t.Logf("IPv6 loopback unavailable (%v); IPv6 exposure assertions skipped", err)
	} else {
		require.NoError(t, tcpEcho(net.JoinHostPort("::1", strconv.Itoa(tcpPort)), "local-loopback-v6"))
		require.Eventually(t, func() bool {
			return udpEcho(net.JoinHostPort("::1", strconv.Itoa(udpPort)), "local-loopback-udp-v6") == nil
		}, 10*time.Second, 50*time.Millisecond, "local IPv6 UDP mapping must recover after the switch")
		if public6 := firstNonLoopbackIP(t, true); public6 != "" &&
			!containsFamily(tcpView.UnavailableFamilies, "ipv6") {
			requireTCPRefused(t, net.JoinHostPort(public6, strconv.Itoa(tcpPort)))
			requireUDPNoEcho(t, net.JoinHostPort(public6, strconv.Itoa(udpPort)))
		} else if containsFamily(tcpView.UnavailableFamilies, "ipv6") {
			t.Logf("IPv6 is unavailable in the public bind (%v); public IPv6 refusal not asserted",
				tcpView.UnavailableFamilies)
		} else {
			t.Logf("no reachable non-loopback IPv6 address; public IPv6 refusal not asserted")
		}
	}

	// A local connection must be closed by the switch back to public, and the
	// public listeners must serve again.
	loopbackConn, err := net.DialTimeout("tcp", loopbackTCP(tcpPort), 2*time.Second)
	require.NoError(t, err)
	defer loopbackConn.Close()
	requireTCPEcho(t, loopbackConn, "before-public-switch")

	publicTCP := setExposureWeb(t, webClient, webAddr, cookie, exposureTestClientID, "tcp", "public")
	require.Equal(t, view.ExposurePublic, publicTCP.Exposure)
	publicUDP := setExposureWeb(t, webClient, webAddr, cookie, exposureTestClientID, "udp", "public")
	require.Equal(t, view.ExposurePublic, publicUDP.Exposure)

	requireConnClosed(t, loopbackConn, 5*time.Second)
	require.Eventually(t, func() bool { return tcpEcho(publicTCP4, "public-again-v4") == nil },
		5*time.Second, 20*time.Millisecond, "public TCP mapping must serve after switching back")
	require.Eventually(t, func() bool { return udpEcho(publicUDP4, "public-again-udp-v4") == nil },
		5*time.Second, 20*time.Millisecond, "public UDP mapping must serve after switching back")
	require.NoError(t, tcpEcho(loopbackTCP(tcpPort), "public-loopback-again"))
	require.NoError(t, udpEcho(loopbackUDP(udpPort), "public-loopback-again-udp"))
	waitServerMapping(t, h.srv, exposureTestClientID, "tcp", 5*time.Second,
		func(m view.MappingView) bool { return m.Status == "running" && m.Exposure == view.ExposurePublic })
	waitClientMapping(t, ec.cl, "tcp", 5*time.Second,
		func(m view.MappingView) bool { return m.Exposure == view.ExposurePublic })
}

// TestMappingExposurePartialFamily occupies the IPv6 loopback port before the
// mapping binds, so the public wildcard can bind IPv4 but not IPv6. The warning
// must reach the server and client read models, IPv4 must keep serving, and a
// retry after the conflict is removed must clear the warning.
func TestMappingExposurePartialFamily(t *testing.T) {
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)

	dir := t.TempDir()
	cfg := newExposureServerConfig(t, dir, nil)
	port := freePortInRange(t, cfg.Ports.Allow[0], "tcp")
	cfg.Ports.Allow = []string{strconv.Itoa(port)}
	cfg.Ports.AutoRange = fmt.Sprintf("%d-%d", port, port)

	fixture, err := net.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable, cannot simulate a one-family bind failure: %v", err)
	}
	t.Cleanup(func() { _ = fixture.Close() })

	clientCfg := newE2EClientConfig(t, t.TempDir(), exposureTestClientID,
		"", "", config.ModeHY2, tcpEchoAddr, "")
	clientCfg.Mappings[0].RemotePort = port

	h := startExposureServer(t, cfg)
	clientCfg.Server = h.srv.ListenAddr().String()
	clientCfg.TLS = config.ClientTLSConfig{CA: h.cfg.TLS.Cert, SNI: "localhost"}
	ec := startE2EClient(t, clientCfg, testFailoverOptions())
	ec.waitReady(t, 10*time.Second)

	partial := waitServerMapping(t, h.srv, exposureTestClientID, "tcp", 10*time.Second,
		func(m view.MappingView) bool {
			return m.Status == "running" && m.AssignedPort == port &&
				containsFamily(m.UnavailableFamilies, "ipv6")
		})
	require.Equal(t, view.ExposurePublic, partial.Exposure)
	require.NotContains(t, partial.UnavailableFamilies, "ipv4", "IPv4 must keep serving")
	waitClientMapping(t, ec.cl, "tcp", 5*time.Second,
		func(m view.MappingView) bool { return containsFamily(m.UnavailableFamilies, "ipv6") })

	require.NoError(t, tcpEcho(loopbackTCP(port), "partial-ipv4"))
	public4 := firstNonLoopbackIP(t, false)
	if public4 != "" {
		require.NoError(t, tcpEcho(net.JoinHostPort(public4, strconv.Itoa(port)), "partial-public-ipv4"))
	}

	require.NoError(t, fixture.Close())
	updated, err := h.srv.SetMappingExposure(exposureTestClientID, "tcp", view.ExposurePublic)
	require.NoError(t, err, "a same-value request must retry an unavailable family")
	require.Empty(t, updated.UnavailableFamilies, "the retry must clear the warning")

	waitServerMapping(t, h.srv, exposureTestClientID, "tcp", 5*time.Second,
		func(m view.MappingView) bool {
			return m.Status == "running" && len(m.UnavailableFamilies) == 0
		})
	waitClientMapping(t, ec.cl, "tcp", 5*time.Second,
		func(m view.MappingView) bool { return len(m.UnavailableFamilies) == 0 })
	require.NoError(t, tcpEcho(net.JoinHostPort("::1", strconv.Itoa(port)), "partial-ipv6-recovered"))
}

// TestMappingExposureReconnect switches a mapping to local, reconnects the
// client, restarts the server from the same state file, and reconnects again.
// Local exposure and the loopback-only listeners must survive every step.
func TestMappingExposureReconnect(t *testing.T) {
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)

	dir := t.TempDir()
	cfg := newExposureServerConfig(t, dir, nil)
	port := freePortInRange(t, cfg.Ports.Allow[0], "tcp")
	cfg.Ports.Allow = []string{strconv.Itoa(port)}
	cfg.Ports.AutoRange = fmt.Sprintf("%d-%d", port, port)
	h := startExposureServer(t, cfg)

	clientCfg := newE2EClientConfig(t, t.TempDir(), exposureTestClientID,
		h.srv.ListenAddr().String(), h.cfg.TLS.Cert, config.ModeHY2, tcpEchoAddr, "")
	clientCfg.Mappings[0].RemotePort = port
	ec := startE2EClient(t, clientCfg, testFailoverOptions())
	ec.waitReady(t, 10*time.Second)

	running := waitServerMapping(t, h.srv, exposureTestClientID, "tcp", 10*time.Second,
		func(m view.MappingView) bool { return m.Status == "running" && m.AssignedPort == port })
	waitClientAssignedPort(t, ec.path, "tcp", port, 5*time.Second)

	_, err := h.srv.SetMappingExposure(exposureTestClientID, "tcp", view.ExposureLocal)
	require.NoError(t, err)
	waitServerMapping(t, h.srv, exposureTestClientID, "tcp", 5*time.Second,
		func(m view.MappingView) bool { return m.Status == "running" && m.Exposure == view.ExposureLocal })
	waitClientMapping(t, ec.cl, "tcp", 5*time.Second,
		func(m view.MappingView) bool { return m.Exposure == view.ExposureLocal })
	requireLocalMappingOnly(t, port, running)

	// Client reconnect without a server restart.
	stopExposureClient(t, ec)
	waitServerOffline(t, h.srv, exposureTestClientID, 5*time.Second)
	saved, err := config.LoadClient(ec.path)
	require.NoError(t, err)
	ec = startE2EClient(t, saved, testFailoverOptions())
	ec.waitReady(t, 10*time.Second)
	waitServerMapping(t, h.srv, exposureTestClientID, "tcp", 5*time.Second,
		func(m view.MappingView) bool { return m.Status == "running" && m.Exposure == view.ExposureLocal })
	waitClientMapping(t, ec.cl, "tcp", 5*time.Second,
		func(m view.MappingView) bool { return m.Exposure == view.ExposureLocal })
	requireLocalMappingOnly(t, port, running)

	// Server restart with the same state file. The offline view must already
	// report local before the client reconnects.
	stopExposureClient(t, ec)
	waitServerOffline(t, h.srv, exposureTestClientID, 5*time.Second)
	h.stop()

	h2 := startExposureServer(t, h.cfg)
	waitServerMapping(t, h2.srv, exposureTestClientID, "tcp", 5*time.Second,
		func(m view.MappingView) bool { return m.Status == "offline" && m.Exposure == view.ExposureLocal })
	require.Eventually(t, func() bool {
		for _, c := range h2.srv.Clients() {
			if c.ID == exposureTestClientID {
				return !c.Online && len(c.Mappings) == 1 && c.Mappings[0].Exposure == view.ExposureLocal
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "the restarted server must load the persisted local exposure")

	saved, err = config.LoadClient(ec.path)
	require.NoError(t, err)
	ec = startE2EClient(t, saved, testFailoverOptions())
	ec.waitReady(t, 10*time.Second)
	waitServerMapping(t, h2.srv, exposureTestClientID, "tcp", 5*time.Second,
		func(m view.MappingView) bool { return m.Status == "running" && m.Exposure == view.ExposureLocal })
	waitClientMapping(t, ec.cl, "tcp", 5*time.Second,
		func(m view.MappingView) bool { return m.Exposure == view.ExposureLocal })
	requireLocalMappingOnly(t, port, running)
}

func requireLocalMappingOnly(t *testing.T, port int, mapping view.MappingView) {
	t.Helper()

	require.NoError(t, tcpEcho(loopbackTCP(port), "local-loopback"))
	if public4 := firstNonLoopbackIP(t, false); public4 != "" &&
		!containsFamily(mapping.UnavailableFamilies, "ipv4") {
		requireTCPRefused(t, net.JoinHostPort(public4, strconv.Itoa(port)))
	}
	if err := ipv6LoopbackUnavailable(); err != nil {
		require.True(t, containsFamily(mapping.UnavailableFamilies, "ipv6"),
			"IPv6 loopback is unavailable, so the public bind must report the ipv6 warning")
		t.Logf("IPv6 loopback unavailable (%v); IPv6 local assertions skipped after asserting the warning", err)
		return
	}
	require.NoError(t, tcpEcho(net.JoinHostPort("::1", strconv.Itoa(port)), "local-loopback-v6"))
	if public6 := firstNonLoopbackIP(t, true); public6 != "" &&
		!containsFamily(mapping.UnavailableFamilies, "ipv6") {
		requireTCPRefused(t, net.JoinHostPort(public6, strconv.Itoa(port)))
	} else if public6 == "" {
		t.Logf("no reachable non-loopback IPv6 address; public IPv6 refusal not asserted")
	}
}

// setExposureWeb drives the real authenticated API and decodes the raw
// view.MappingView success body.
func setExposureWeb(t *testing.T, client *http.Client, webAddr string, cookie *http.Cookie,
	clientID, mappingID, exposure string) view.MappingView {
	t.Helper()

	body, err := json.Marshal(map[string]string{"exposure": exposure})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPut,
		"http://"+webAddr+"/api/clients/"+url.PathEscape(clientID)+
			"/mappings/"+url.PathEscape(mappingID)+"/exposure",
		strings.NewReader(string(body)))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "hy2tun")
	req.Header.Set("Origin", "http://"+webAddr)
	req.AddCookie(cookie)

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(resp.Body)
		t.Fatalf("set exposure %s/%s=%s: status %d: %s", clientID, mappingID, exposure, resp.StatusCode, message)
	}
	var result view.MappingView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	return result
}

func requireConnClosed(t *testing.T, conn net.Conn, timeout time.Duration) {
	t.Helper()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(timeout)))
	buf := make([]byte, 1)
	_, err := conn.Read(buf)
	require.Error(t, err, "an exposure switch must close the existing connection")
}

// isConnectionRefused uses the platform error text so the E2E helper does not
// depend on a Unix-only syscall constant. Windows reports "actively refused".
func isConnectionRefused(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "refused")
}

func requireTCPRefused(t *testing.T, addr string) {
	t.Helper()

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err != nil {
			return isConnectionRefused(err)
		}
		_ = conn.Close()
		return false
	}, 5*time.Second, 20*time.Millisecond, "public TCP address %s must refuse connections", addr)
}

func requireUDPNoEcho(t *testing.T, addr string) {
	t.Helper()

	require.Eventually(t, func() bool { return udpEcho(addr, "must-not-echo") != nil },
		3*time.Second, 50*time.Millisecond, "public UDP address %s must not answer", addr)
}

func loopbackTCP(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

func loopbackUDP(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

func containsFamily(families []string, family string) bool {
	for _, candidate := range families {
		if candidate == family {
			return true
		}
	}
	return false
}

func newExposureServerConfig(t *testing.T, dir string, configure func(*config.ServerConfig)) *config.ServerConfig {
	t.Helper()

	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	certPath, keyPath := writeCertificate(t, dir, cert)
	portRange := nextE2EPortRange()
	cfg := &config.ServerConfig{
		Listen:     freeUDPListenAddr(t),
		ListenHost: "",
		Auth:       config.AuthConfig{Type: "password", Password: e2ePassword},
		TLS:        config.TLSConfig{Cert: certPath, Key: keyPath},
		Control:    config.ControlConfig{Token: e2eControlToken},
		Ports:      config.PortsConfig{Allow: []string{portRange}, AutoRange: portRange},
		Web:        config.WebConfig{Listen: ""},
		StateFile:  filepath.Join(dir, "state.json"),
		Log:        config.LogConfig{Level: "error"},
		ConfigPath: filepath.Join(dir, "server.yaml"),
	}
	if configure != nil {
		configure(cfg)
	}
	return cfg
}

type exposureServer struct {
	t      *testing.T
	cfg    *config.ServerConfig
	srv    *server.Server
	cancel context.CancelFunc
	done   chan error
}

func startExposureServer(t *testing.T, cfg *config.ServerConfig) *exposureServer {
	t.Helper()

	require.NoError(t, config.SaveServer(cfg.ConfigPath, cfg))
	loaded, err := config.LoadServer(cfg.ConfigPath)
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := server.New(loaded, logger)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()
	h := &exposureServer{t: t, cfg: loaded, srv: srv, cancel: cancel, done: done}
	t.Cleanup(h.stop)
	return h
}

func (h *exposureServer) stop() {
	if h == nil || h.cancel == nil {
		return
	}
	h.cancel()
	if err := h.srv.Close(); err != nil {
		h.t.Errorf("close exposure server: %v", err)
	}
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("exposure server Start returned %v", err)
		}
	case <-time.After(5 * time.Second):
		h.t.Error("exposure server did not stop after Close")
	}
	h.cancel = nil
}

func freeUDPListenAddr(t *testing.T) string {
	t.Helper()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := pc.LocalAddr().String()
	require.NoError(t, pc.Close())
	return addr
}

func waitServerMapping(t *testing.T, srv *server.Server, clientID, mappingID string,
	timeout time.Duration, check func(view.MappingView) bool) view.MappingView {
	t.Helper()

	var result view.MappingView
	require.Eventually(t, func() bool {
		for _, c := range srv.Clients() {
			if c.ID != clientID {
				continue
			}
			for _, mapping := range c.Mappings {
				if mapping.ID == mappingID && check(mapping) {
					result = mapping
					return true
				}
			}
		}
		return false
	}, timeout, 20*time.Millisecond)
	return result
}

func waitClientMapping(t *testing.T, cl *client.Client, mappingID string,
	timeout time.Duration, check func(view.MappingView) bool) view.MappingView {
	t.Helper()

	var result view.MappingView
	require.Eventually(t, func() bool {
		for _, mapping := range cl.Mappings() {
			if mapping.ID == mappingID && check(mapping) {
				result = mapping
				return true
			}
		}
		return false
	}, timeout, 20*time.Millisecond)
	return result
}

func waitServerOffline(t *testing.T, srv *server.Server, clientID string, timeout time.Duration) {
	t.Helper()

	require.Eventually(t, func() bool {
		for _, c := range srv.Clients() {
			if c.ID == clientID {
				return !c.Online
			}
		}
		return false
	}, timeout, 20*time.Millisecond, "client %s must go offline", clientID)
}

func stopExposureClient(t *testing.T, ec *e2eClient) {
	t.Helper()

	ec.cancel()
	require.NoError(t, ec.cl.Close())
}

func waitClientAssignedPort(t *testing.T, path, mappingID string, port int, timeout time.Duration) {
	t.Helper()

	require.Eventually(t, func() bool {
		cfg, err := config.LoadClient(path)
		if err != nil {
			return false
		}
		for _, mapping := range cfg.Mappings {
			if mapping.ID == mappingID && mapping.RemotePort == port {
				return true
			}
		}
		return false
	}, timeout, 20*time.Millisecond, "client must persist the assigned port")
}

func waitClientRevisionStable(t *testing.T, path string, timeout time.Duration) int64 {
	t.Helper()

	var (
		last   int64 = -1
		stable int
	)
	require.Eventually(t, func() bool {
		cfg, err := config.LoadClient(path)
		if err != nil {
			return false
		}
		if cfg.Client.Revision == last {
			stable++
		} else {
			last = cfg.Client.Revision
			stable = 0
		}
		return stable >= 2
	}, timeout, 20*time.Millisecond, "client config revision must settle")
	return last
}

func firstNonLoopbackIP(t *testing.T, v6 bool) string {
	t.Helper()

	interfaces, err := net.Interfaces()
	require.NoError(t, err)
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if v6 {
				if ip.To4() != nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
					continue
				}
			} else if ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			return ip.String()
		}
	}
	return ""
}

func ipv6LoopbackUnavailable() error {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		return err
	}
	if err := ln.Close(); err != nil {
		return err
	}
	pc, err := net.ListenPacket("udp6", "[::1]:0")
	if err != nil {
		return err
	}
	return pc.Close()
}

func freePortInRange(t *testing.T, spec, network string) int {
	t.Helper()

	start, end := parseExposurePortRange(t, spec)
	for port := start; port <= end; port++ {
		if exposurePortFree(network, port) {
			return port
		}
	}
	t.Fatalf("no free %s port in %s", network, spec)
	return 0
}

func freePortForBoth(t *testing.T, spec string) int {
	t.Helper()

	start, end := parseExposurePortRange(t, spec)
	for port := start; port <= end; port++ {
		if exposurePortFree("tcp", port) && exposurePortFree("udp", port) {
			return port
		}
	}
	t.Fatalf("no port in %s is free for both TCP and UDP", spec)
	return 0
}

func parseExposurePortRange(t *testing.T, spec string) (int, int) {
	t.Helper()

	parts := strings.SplitN(spec, "-", 2)
	start, err := strconv.Atoi(parts[0])
	require.NoError(t, err)
	end := start
	if len(parts) == 2 {
		end, err = strconv.Atoi(parts[1])
		require.NoError(t, err)
	}
	return start, end
}

func exposurePortFree(network string, port int) bool {
	if ipv6LoopbackUnavailable() == nil {
		switch network {
		case "tcp":
			ln, err := net.Listen("tcp6", fmt.Sprintf("[::]:%d", port))
			if err != nil {
				return false
			}
			_ = ln.Close()
		case "udp":
			pc, err := net.ListenPacket("udp6", fmt.Sprintf("[::]:%d", port))
			if err != nil {
				return false
			}
			_ = pc.Close()
		}
	}
	switch network {
	case "tcp":
		ln, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			return false
		}
		return ln.Close() == nil
	case "udp":
		pc, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", port))
		if err != nil {
			return false
		}
		return pc.Close() == nil
	}
	return false
}
