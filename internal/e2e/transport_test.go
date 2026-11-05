package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"
	xproxy "golang.org/x/net/proxy"

	"hy2tun/internal/client"
	"hy2tun/internal/config"
	"hy2tun/internal/server"
	"hy2tun/internal/testutil"
	"hy2tun/internal/transport"
	"hy2tun/internal/view"
)

// e2ePortRangeSeq gives each server in this package a distinct high range so
// auto-allocated mapping ports do not collide across tests.
var e2ePortRangeSeq atomic.Int64

func nextE2EPortRange() string {
	n := e2ePortRangeSeq.Add(1)
	base := 45000 + int(n%80)*100
	return fmt.Sprintf("%d-%d", base, base+99)
}

// testFailoverOptions shortens the real timers only for E2E workers. Production
// defaults are untouched; the policy thresholds themselves are pinned by the
// fake-clock unit tests.
func testFailoverOptions() client.RuntimeOptions {
	return client.RuntimeOptions{
		Policy: client.FailoverPolicy{
			ConnectTimeout:    time.Second,
			ProbeInterval:     200 * time.Millisecond,
			MinTCPHold:        500 * time.Millisecond,
			RecoverySuccesses: 3,
		},
		Jitter: func(d time.Duration) time.Duration { return d },
	}
}

type e2eServer struct {
	srv        *server.Server
	certPath   string
	dir        string
	configPath string
}

func startE2EServer(t *testing.T, mode config.TransportMode, portRange string, configure ...func(*config.ServerConfig)) *e2eServer {
	t.Helper()

	dir := t.TempDir()
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	certPath, keyPath := writeCertificate(t, dir, cert)

	cfg := &config.ServerConfig{
		Mode:       mode,
		Listen:     "127.0.0.1:0",
		ListenHost: "127.0.0.1",
		Auth:       config.AuthConfig{Type: "password", Password: e2ePassword},
		TLS:        config.TLSConfig{Cert: certPath, Key: keyPath},
		Control:    config.ControlConfig{Token: e2eControlToken},
		Ports:      config.PortsConfig{Allow: []string{portRange}, AutoRange: portRange},
		Web:        config.WebConfig{Listen: ""},
		StateFile:  filepath.Join(dir, "state.json"),
		Log:        config.LogConfig{Level: "info"},
		ConfigPath: filepath.Join(dir, "server.yaml"),
	}
	for _, fn := range configure {
		fn(cfg)
	}
	require.NoError(t, config.SaveServer(cfg.ConfigPath, cfg))
	loaded, err := config.LoadServer(cfg.ConfigPath)
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := server.New(loaded, logger)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	es := &e2eServer{srv: srv, certPath: certPath, dir: dir, configPath: cfg.ConfigPath}
	t.Cleanup(func() {
		cancel()
		if err := srv.Close(); err != nil {
			t.Errorf("close e2e server: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("e2e server Start returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("e2e server Start did not return after Close")
		}
	})
	return es
}

type e2eClient struct {
	cl     *client.Client
	cfg    *config.ClientConfig
	path   string
	cancel context.CancelFunc
	done   chan error
}

func startE2EClient(t *testing.T, cfg *config.ClientConfig, opts client.RuntimeOptions) *e2eClient {
	t.Helper()

	require.NotEmpty(t, cfg.ConfigPath, "test client config path is required")
	require.NoError(t, config.SaveClient(cfg.ConfigPath, cfg))
	loaded, err := config.LoadClient(cfg.ConfigPath)
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cl, err := client.NewWithRuntime(loaded, logger, opts)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cl.Start(ctx) }()

	ec := &e2eClient{cl: cl, cfg: loaded, path: cfg.ConfigPath, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		_ = cl.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("e2e client Start did not return after Close")
		}
	})
	return ec
}

func (c *e2eClient) waitReady(t *testing.T, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	require.NoError(t, c.cl.WaitReady(ctx))
}

func newE2EClientConfig(t *testing.T, dir, id, serverAddr, certPath string, mode config.TransportMode, tcpEcho, udpEcho string) *config.ClientConfig {
	t.Helper()

	cfg := &config.ClientConfig{
		Mode:       mode,
		Server:     serverAddr,
		Auth:       e2ePassword,
		TLS:        config.ClientTLSConfig{CA: certPath, SNI: "localhost"},
		Control:    config.ControlConfig{Token: e2eControlToken},
		Client:     config.ClientIdentity{ID: id, Name: id},
		Web:        config.WebConfig{Listen: ""},
		Log:        config.LogConfig{Level: "info"},
		Mappings:   []config.Mapping{{ID: "tcp", Type: "tcp", LocalAddr: tcpEcho, Enabled: true}},
		ConfigPath: filepath.Join(dir, "client.yaml"),
	}
	if udpEcho != "" {
		cfg.Mappings = append(cfg.Mappings, config.Mapping{ID: "udp", Type: "udp", LocalAddr: udpEcho, Enabled: true})
	}
	return cfg
}

func waitClientTransport(t *testing.T, cl *client.Client, want string, timeout time.Duration) view.TransportState {
	t.Helper()
	var state view.TransportState
	require.Eventually(t, func() bool {
		state = cl.TransportState()
		return state.ActiveTransport == want
	}, timeout, 20*time.Millisecond, "client transport must become %q", want)
	return state
}

func waitServerClient(t *testing.T, srv *server.Server, id, wantTransport string, wantMappings int, timeout time.Duration) view.ClientView {
	t.Helper()
	var (
		last         view.ClientView
		lastRevision int64 = -1
		stablePolls  int
	)
	require.Eventually(t, func() bool {
		views := srv.Clients()
		if len(views) != 1 {
			lastRevision, stablePolls = -1, 0
			return false
		}
		candidate := views[0]
		if candidate.ID != id || !candidate.Online || candidate.Transport != wantTransport || candidate.Revision == 0 {
			lastRevision, stablePolls = -1, 0
			return false
		}
		if wantMappings >= 0 && len(candidate.Mappings) != wantMappings {
			lastRevision, stablePolls = -1, 0
			return false
		}
		if len(candidate.Mappings) == 0 {
			lastRevision, stablePolls = -1, 0
			return false
		}
		for _, mapping := range candidate.Mappings {
			if mapping.Status != "running" || mapping.AssignedPort == 0 {
				lastRevision, stablePolls = -1, 0
				return false
			}
		}
		if candidate.Revision != lastRevision {
			lastRevision, stablePolls = candidate.Revision, 1
			return false
		}
		stablePolls++
		if stablePolls < 2 {
			return false
		}
		last = candidate
		return true
	}, timeout, 20*time.Millisecond, "server must show %q over %s with running mappings", id, wantTransport)
	return last
}

func waitClientMappings(t *testing.T, cl *client.Client, want int, timeout time.Duration) []view.MappingView {
	t.Helper()
	var last []view.MappingView
	require.Eventually(t, func() bool {
		views := cl.Mappings()
		if len(views) != want {
			return false
		}
		for _, mapping := range views {
			if mapping.Status != "running" || mapping.AssignedPort == 0 {
				return false
			}
		}
		last = views
		return true
	}, timeout, 20*time.Millisecond, "client must report %d running mappings", want)
	return last
}

func mappingByID(t *testing.T, mappings []view.MappingView, id string) view.MappingView {
	t.Helper()
	for _, mapping := range mappings {
		if mapping.ID == id {
			return mapping
		}
	}
	t.Fatalf("mapping %q not found", id)
	return view.MappingView{}
}

func publicAddr(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// TestTransportModeNineCombinations runs the real 3x3 server/client mode
// matrix. Seven pairs share a transport and must work; pure HY2 against pure
// TCP (and the reverse) must fail without silently using the other transport.
func TestTransportModeNineCombinations(t *testing.T) {
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)
	udpEchoAddr, _ := testutil.StartUDPEcho(t)

	type testCase struct {
		server   config.TransportMode
		client   config.TransportMode
		wantOK   bool
		wantKind string
		forbid   string
	}
	cases := []testCase{
		{config.ModeHY2, config.ModeHY2, true, "hy2", ""},
		{config.ModeHY2, config.ModeAuto, true, "hy2", ""},
		{config.ModeHY2, config.ModeTCP, false, "", "hy2"},
		{config.ModeAuto, config.ModeHY2, true, "hy2", ""},
		{config.ModeAuto, config.ModeAuto, true, "hy2", ""},
		{config.ModeAuto, config.ModeTCP, true, "tcp", ""},
		{config.ModeTCP, config.ModeHY2, false, "", "tcp"},
		{config.ModeTCP, config.ModeAuto, true, "tcp", ""},
		{config.ModeTCP, config.ModeTCP, true, "tcp", ""},
	}

	for _, tc := range cases {
		name := fmt.Sprintf("server-%s_client-%s", tc.server, tc.client)
		t.Run(name, func(t *testing.T) {
			es := startE2EServer(t, tc.server, nextE2EPortRange())
			cfg := newE2EClientConfig(t, t.TempDir(), "matrix-"+name, es.srv.ListenAddr().String(),
				es.certPath, tc.client, tcpEchoAddr, udpEchoAddr)
			ec := startE2EClient(t, cfg, testFailoverOptions())

			if !tc.wantOK {
				readyCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				err := ec.cl.WaitReady(readyCtx)
				cancel()
				require.Error(t, err, "mutually exclusive pure modes must not connect")
				require.Empty(t, es.srv.Clients(), "mutually exclusive pure modes must not register")
				require.NotEqual(t, tc.forbid, ec.cl.TransportState().ActiveTransport,
					"a pure mode must never silently downgrade to the other transport")

				saved, err := config.LoadClient(cfg.ConfigPath)
				require.NoError(t, err)
				require.Equal(t, tc.client, saved.Mode, "failed transport attempts must not rewrite the configured mode")
				return
			}

			ec.waitReady(t, 10*time.Second)
			waitClientTransport(t, ec.cl, tc.wantKind, 10*time.Second)
			serverView := waitServerClient(t, es.srv, cfg.Client.ID, tc.wantKind, 2, 10*time.Second)

			tcpPort := mappingByID(t, serverView.Mappings, "tcp").AssignedPort
			udpPort := mappingByID(t, serverView.Mappings, "udp").AssignedPort
			require.NoError(t, tcpEcho(publicAddr(tcpPort), "matrix-tcp"))
			require.Eventually(t, func() bool {
				return udpEcho(publicAddr(udpPort), "matrix-udp") == nil
			}, 5*time.Second, 20*time.Millisecond, "reverse UDP must work over the shared transport")

			saved, err := config.LoadClient(cfg.ConfigPath)
			require.NoError(t, err)
			require.Equal(t, tc.client, saved.Mode, "actual transport must not be persisted as the configured mode")
		})
	}
}

// TestUDPBlockedFallbackAndRecovery blocks the gate's UDP side so HY2 cannot
// connect, verifies both TCP and UDP mappings over the TCP fallback, observes
// isolated probes that must not mutate registration, then restores UDP and
// requires the actual transport to return to HY2 with a single client session.
func TestUDPBlockedFallbackAndRecovery(t *testing.T) {
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)
	udpEchoAddr, _ := testutil.StartUDPEcho(t)

	es := startE2EServer(t, config.ModeAuto, nextE2EPortRange())
	gate := newTransportGate(t, es.srv.ListenAddr().String())
	gate.SetUDPEnabled(false)

	cfg := newE2EClientConfig(t, t.TempDir(), "udp-blocked", gate.Addr(), es.certPath,
		config.ModeAuto, tcpEchoAddr, udpEchoAddr)
	ec := startE2EClient(t, cfg, testFailoverOptions())

	ec.waitReady(t, 10*time.Second)
	state := waitClientTransport(t, ec.cl, "tcp", 10*time.Second)
	require.Equal(t, uint64(0), state.SwitchCount, "the first accepted transport is not a switch")
	require.Equal(t, "hy2_unavailable", state.SwitchReason)

	serverView := waitServerClient(t, es.srv, cfg.Client.ID, "tcp", 2, 10*time.Second)
	tcpPort := mappingByID(t, serverView.Mappings, "tcp").AssignedPort
	udpPort := mappingByID(t, serverView.Mappings, "udp").AssignedPort
	revision := serverView.Revision

	require.NoError(t, tcpEcho(publicAddr(tcpPort), "tcp-over-fallback"))
	require.Eventually(t, func() bool {
		return udpEcho(publicAddr(udpPort), "udp-over-fallback") == nil
	}, 5*time.Second, 20*time.Millisecond, "UDP echo must work over the TCP datagram bridge")

	// Wait for a real probe to fail, then for a second probe to happen. This is
	// synchronization on the policy loop rather than a sleep used as proof.
	var firstProbe time.Time
	require.Eventually(t, func() bool {
		st := ec.cl.TransportState()
		if st.LastProbeAt.IsZero() || st.LastProbeError == "" {
			return false
		}
		firstProbe = st.LastProbeAt
		return true
	}, 5*time.Second, 20*time.Millisecond, "the blocked path must produce a failed probe")
	require.Eventually(t, func() bool {
		return ec.cl.TransportState().LastProbeAt.After(firstProbe)
	}, 5*time.Second, 20*time.Millisecond, "the probe loop must keep running while TCP carries traffic")

	// A probe is read-only: no extra client, no mapping or revision changes.
	require.Len(t, es.srv.Clients(), 1, "probes must not create a second client")
	probed := es.srv.Clients()[0]
	require.Equal(t, revision, probed.Revision, "probes must not change the mapping revision")
	require.Equal(t, tcpPort, mappingByID(t, probed.Mappings, "tcp").AssignedPort)
	require.Equal(t, udpPort, mappingByID(t, probed.Mappings, "udp").AssignedPort)
	require.Equal(t, "tcp", probed.Transport)
	require.Equal(t, "tcp", ec.cl.TransportState().ActiveTransport)

	saved, err := config.LoadClient(cfg.ConfigPath)
	require.NoError(t, err)
	require.Equal(t, config.ModeAuto, saved.Mode, "fallback must not persist the actual TCP transport as the mode")

	gate.SetUDPEnabled(true)

	recovered := waitClientTransport(t, ec.cl, "hy2", 10*time.Second)
	require.Equal(t, "hy2_recovered", recovered.SwitchReason)
	require.Equal(t, uint64(1), recovered.SwitchCount)
	require.Empty(t, recovered.LastProbeError)

	after := waitServerClient(t, es.srv, cfg.Client.ID, "hy2", 2, 10*time.Second)
	require.Equal(t, revision, after.Revision, "recovery must reuse the latest revision")
	require.Equal(t, tcpPort, mappingByID(t, after.Mappings, "tcp").AssignedPort)
	require.Equal(t, udpPort, mappingByID(t, after.Mappings, "udp").AssignedPort)
	require.Len(t, es.srv.Clients(), 1, "recovery must leave exactly one client session")

	require.NoError(t, tcpEcho(publicAddr(tcpPort), "tcp-after-recovery"))
	require.Eventually(t, func() bool {
		return udpEcho(publicAddr(udpPort), "udp-after-recovery") == nil
	}, 5*time.Second, 20*time.Millisecond, "reverse UDP must work after recovery")

	saved, err = config.LoadClient(cfg.ConfigPath)
	require.NoError(t, err)
	require.Equal(t, config.ModeAuto, saved.Mode, "recovery must not rewrite the configured mode")
}

// TestMultiTransportIsolation gates only server A. Server B must stay on HY2
// and keep serving while A falls back to TCP and probes in the background.
func TestMultiTransportIsolation(t *testing.T) {
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)
	udpEchoAddr, _ := testutil.StartUDPEcho(t)

	serverA := startE2EServer(t, config.ModeAuto, nextE2EPortRange())
	serverB := startE2EServer(t, config.ModeAuto, nextE2EPortRange())
	gateA := newTransportGate(t, serverA.srv.ListenAddr().String())
	gateA.SetUDPEnabled(false)

	cfgA := newE2EClientConfig(t, t.TempDir(), "isolation-a", gateA.Addr(), serverA.certPath,
		config.ModeAuto, tcpEchoAddr, udpEchoAddr)
	cfgB := newE2EClientConfig(t, t.TempDir(), "isolation-b", serverB.srv.ListenAddr().String(), serverB.certPath,
		config.ModeAuto, tcpEchoAddr, udpEchoAddr)
	clientA := startE2EClient(t, cfgA, testFailoverOptions())
	clientB := startE2EClient(t, cfgB, testFailoverOptions())

	clientA.waitReady(t, 10*time.Second)
	clientB.waitReady(t, 10*time.Second)
	waitClientTransport(t, clientA.cl, "tcp", 10*time.Second)
	waitClientTransport(t, clientB.cl, "hy2", 10*time.Second)

	viewA := waitServerClient(t, serverA.srv, cfgA.Client.ID, "tcp", 2, 10*time.Second)
	viewB := waitServerClient(t, serverB.srv, cfgB.Client.ID, "hy2", 2, 10*time.Second)

	portA := mappingByID(t, viewA.Mappings, "tcp").AssignedPort
	portB := mappingByID(t, viewB.Mappings, "tcp").AssignedPort
	udpA := mappingByID(t, viewA.Mappings, "udp").AssignedPort
	udpB := mappingByID(t, viewB.Mappings, "udp").AssignedPort

	require.NoError(t, tcpEcho(publicAddr(portA), "isolation-a-tcp"))
	require.NoError(t, tcpEcho(publicAddr(portB), "isolation-b-tcp"))
	require.Eventually(t, func() bool { return udpEcho(publicAddr(udpA), "isolation-a-udp") == nil },
		5*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool { return udpEcho(publicAddr(udpB), "isolation-b-udp") == nil },
		5*time.Second, 20*time.Millisecond)

	// Wait until A has really probed; B must remain untouched.
	var probeAt time.Time
	require.Eventually(t, func() bool {
		st := clientA.cl.TransportState()
		if st.LastProbeAt.IsZero() {
			return false
		}
		probeAt = st.LastProbeAt
		return true
	}, 5*time.Second, 20*time.Millisecond)
	require.Eventually(t, func() bool {
		return clientA.cl.TransportState().LastProbeAt.After(probeAt)
	}, 5*time.Second, 20*time.Millisecond)

	require.Equal(t, "tcp", clientA.cl.TransportState().ActiveTransport)
	require.Equal(t, "hy2", clientB.cl.TransportState().ActiveTransport)
	require.Zero(t, clientB.cl.TransportState().SwitchCount, "the unaffected server must never switch")
	require.Equal(t, "hy2", waitServerClient(t, serverB.srv, cfgB.Client.ID, "hy2", 2, 5*time.Second).Transport)

	require.NoError(t, tcpEcho(publicAddr(portB), "isolation-b-survives"))
	require.Eventually(t, func() bool { return udpEcho(publicAddr(udpB), "isolation-b-udp-survives") == nil },
		5*time.Second, 20*time.Millisecond)
}

// TestFallbackPushRotationAndProxy exercises the TCP transport's control plane
// end to end: web push/delete, credential rotation, and both local proxies.
func TestFallbackPushRotationAndProxy(t *testing.T) {
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)
	udpEchoAddr, _ := testutil.StartUDPEcho(t)

	hash, err := bcryptHash(e2eWebPassword)
	require.NoError(t, err)
	es := startE2EServer(t, config.ModeAuto, nextE2EPortRange(), func(cfg *config.ServerConfig) {
		cfg.Web = config.WebConfig{Listen: "127.0.0.1:0", Username: "admin", PasswordHash: hash}
	})
	gate := newTransportGate(t, es.srv.ListenAddr().String())
	gate.SetUDPEnabled(false)

	cfg := newE2EClientConfig(t, t.TempDir(), "fallback-push", gate.Addr(), es.certPath,
		config.ModeAuto, tcpEchoAddr, udpEchoAddr)
	cfg.Proxy = config.ProxyConfig{
		Enabled: true,
		SOCKS5:  freeLocalTCPAddress(t),
		HTTP:    freeLocalTCPAddress(t),
	}
	ec := startE2EClient(t, cfg, testFailoverOptions())

	ec.waitReady(t, 10*time.Second)
	waitClientTransport(t, ec.cl, "tcp", 10*time.Second)
	serverView := waitServerClient(t, es.srv, cfg.Client.ID, "tcp", 2, 10*time.Second)
	baseTCP := mappingByID(t, serverView.Mappings, "tcp").AssignedPort

	requireSOCKSEcho(t, cfg.Proxy.SOCKS5, tcpEchoAddr, "socks-over-fallback")
	requireHTTPConnectEcho(t, cfg.Proxy.HTTP, tcpEchoAddr, "http-over-fallback")

	webAddr := waitForWeb(t, es.srv)
	webClient := &http.Client{Timeout: 10 * time.Second}
	cookie := loginWeb(t, webClient, webAddr)

	clients := listWebClients(t, webClient, webAddr, cookie)
	require.Len(t, clients, 1)
	require.Equal(t, cfg.Client.ID, clients[0].ID)
	require.Equal(t, "tcp", clients[0].Transport)

	baseMappings := mappingsFromView(clients[0].Mappings)
	require.Len(t, baseMappings, 2)
	pushed := config.Mapping{ID: "fallback-pushed", Type: "tcp", LocalAddr: tcpEchoAddr, Enabled: true}
	pushRevision := pushMappings(t, webClient, webAddr, cookie, clients[0].ID, clients[0].Revision,
		append(baseMappings, pushed))
	require.Equal(t, clients[0].Revision+1, pushRevision)

	var pushedMapping config.Mapping
	require.Eventually(t, func() bool {
		saved, err := config.LoadClient(cfg.ConfigPath)
		if err != nil {
			return false
		}
		if saved.Client.Revision != pushRevision {
			return false
		}
		for _, mapping := range saved.Mappings {
			if mapping.ID == pushed.ID {
				pushedMapping = mapping
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "the TCP client must persist a pushed mapping")
	require.NotZero(t, pushedMapping.RemotePort)
	require.Eventually(t, func() bool {
		return tcpEcho(publicAddr(pushedMapping.RemotePort), "pushed-over-fallback") == nil
	}, 5*time.Second, 20*time.Millisecond, "the pushed mapping must be live over TCP")

	deleteRevision := pushMappings(t, webClient, webAddr, cookie, clients[0].ID, pushRevision, baseMappings)
	require.Equal(t, pushRevision+1, deleteRevision)
	require.Eventually(t, func() bool {
		saved, err := config.LoadClient(cfg.ConfigPath)
		if err != nil {
			return false
		}
		if saved.Client.Revision != deleteRevision || len(saved.Mappings) != 2 {
			return false
		}
		for _, mapping := range saved.Mappings {
			if mapping.ID == pushed.ID {
				return false
			}
		}
		return true
	}, 5*time.Second, 20*time.Millisecond, "the TCP client must persist the delete")
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", publicAddr(pushedMapping.RemotePort), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return false
		}
		return errors.Is(err, syscall.ECONNREFUSED)
	}, 5*time.Second, 20*time.Millisecond, "deleting over TCP must close the public port")

	const (
		rotatedAuth  = "hy2tun-e2e-tcp-rotated-password"
		rotatedToken = "00112233445566778899aabbccddeeff"
	)
	generation, results := rotateCredentialsWeb(t, webClient, webAddr, cookie,
		e2eWebPassword, rotatedAuth, rotatedToken, "auto")
	require.Equal(t, int64(2), generation)
	require.Len(t, results, 1)
	require.Equal(t, "synced", results[0].Status)

	require.Eventually(t, func() bool {
		saved, err := config.LoadClient(cfg.ConfigPath)
		if err != nil {
			return false
		}
		return saved.Credentials.Generation == generation && saved.Auth == rotatedAuth &&
			saved.Control.Token == rotatedToken
	}, 10*time.Second, 20*time.Millisecond, "the TCP client must persist rotated credentials")

	require.Eventually(t, func() bool {
		views := es.srv.Clients()
		if len(views) != 1 {
			return false
		}
		c := views[0]
		return c.Online && c.Transport == "tcp" && c.CredentialGeneration == generation &&
			c.CredentialSynced && len(c.Mappings) == 2
	}, 10*time.Second, 20*time.Millisecond, "the TCP client must re-register after rotation")

	require.Eventually(t, func() bool {
		return tcpEcho(publicAddr(baseTCP), "tcp-after-tcp-rotation") == nil
	}, 5*time.Second, 20*time.Millisecond, "the TCP mapping must recover after credential rotation")
	require.Eventually(t, func() bool {
		return udpEcho(publicAddr(mappingByID(t, serverView.Mappings, "udp").AssignedPort), "udp-after-tcp-rotation") == nil
	}, 5*time.Second, 20*time.Millisecond, "the UDP mapping must recover after credential rotation")
	requireSOCKSEcho(t, cfg.Proxy.SOCKS5, tcpEchoAddr, "socks-after-rotation")
	requireHTTPConnectEcho(t, cfg.Proxy.HTTP, tcpEchoAddr, "http-after-rotation")

	saved, err := config.LoadClient(cfg.ConfigPath)
	require.NoError(t, err)
	require.Equal(t, config.ModeAuto, saved.Mode, "rotation must not persist the actual transport as mode")
}

func requireSOCKSEcho(t *testing.T, proxyAddr, target, message string) {
	t.Helper()
	dialer, err := xproxy.SOCKS5("tcp", proxyAddr, nil, xproxy.Direct)
	require.NoError(t, err)
	conn, err := dialer.Dial("tcp", target)
	require.NoError(t, err)
	defer conn.Close()
	requireEchoConn(t, conn, message)
}

func requireHTTPConnectEcho(t *testing.T, proxyAddr, target, message string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, time.Second)
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	requireEchoConn(t, conn, message)
}

// TestFallbackBackpressureCloseBounded floods a UDP mapping whose local target
// does not read while the TCP fallback carries the traffic. Control updates must
// still complete, the mapping must stay bounded/usable, and Close must release
// the public port promptly.
func TestFallbackBackpressureCloseBounded(t *testing.T) {
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)

	sink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	require.NoError(t, sink.SetReadBuffer(4096))
	t.Cleanup(func() { _ = sink.Close() })
	sinkAddr := sink.LocalAddr().String()

	es := startE2EServer(t, config.ModeAuto, nextE2EPortRange())
	gate := newTransportGate(t, es.srv.ListenAddr().String())
	gate.SetUDPEnabled(false)

	cfg := newE2EClientConfig(t, t.TempDir(), "fallback-backpressure", gate.Addr(), es.certPath,
		config.ModeAuto, tcpEchoAddr, sinkAddr)
	ec := startE2EClient(t, cfg, testFailoverOptions())

	ec.waitReady(t, 10*time.Second)
	waitClientTransport(t, ec.cl, "tcp", 10*time.Second)
	serverView := waitServerClient(t, es.srv, cfg.Client.ID, "tcp", 2, 10*time.Second)
	publicUDP := mappingByID(t, serverView.Mappings, "udp").AssignedPort
	publicTCP := mappingByID(t, serverView.Mappings, "tcp").AssignedPort

	stopFlood := make(chan struct{})
	var floodWG sync.WaitGroup
	var sent atomic.Int64
	for i := 0; i < 4; i++ {
		floodWG.Add(1)
		go func() {
			defer floodWG.Done()
			conn, err := net.Dial("udp", publicAddr(publicUDP))
			if err != nil {
				return
			}
			defer conn.Close()
			payload := bytes.Repeat([]byte{0x5a}, 1024)
			for {
				select {
				case <-stopFlood:
					return
				default:
					_ = conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
					if _, err := conn.Write(payload); err == nil {
						sent.Add(1)
					}
				}
			}
		}()
	}
	require.Eventually(t, func() bool { return sent.Load() > 2000 }, 5*time.Second, 20*time.Millisecond,
		"the flood must put real load on the TCP datagram path")

	controlDone := make(chan error, 1)
	go func() {
		controlDone <- ec.cl.AddMapping(config.Mapping{
			ID: "control-during-flood", Type: "tcp", LocalAddr: tcpEchoAddr, Enabled: true,
		})
	}()
	select {
	case err := <-controlDone:
		require.NoError(t, err, "control updates must still be handled during UDP backpressure")
	case <-time.After(5 * time.Second):
		t.Fatal("control update blocked behind UDP backpressure")
	}

	require.Eventually(t, func() bool {
		views := es.srv.Clients()
		if len(views) != 1 || views[0].Transport != "tcp" {
			return false
		}
		for _, mapping := range views[0].Mappings {
			if mapping.ID == "control-during-flood" {
				return mapping.Status == "running" && mapping.AssignedPort != 0
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "the control-created mapping must register during the flood")

	close(stopFlood)
	floodWG.Wait()

	// The queue is bounded by transport (32 frames); the E2E assertion is that
	// overload drops packets but leaves the control plane and mapping alive.
	require.Len(t, es.srv.Clients(), 1)
	require.Equal(t, "running", mappingByID(t, es.srv.Clients()[0].Mappings, "udp").Status)
	require.NoError(t, tcpEcho(publicAddr(publicTCP), "tcp-after-flood"))

	closeDone := make(chan error, 1)
	go func() { closeDone <- ec.cl.Close() }()
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("client Close did not return during UDP backpressure")
	}

	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", publicAddr(publicTCP), 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return false
		}
		return errors.Is(err, syscall.ECONNREFUSED)
	}, 5*time.Second, 20*time.Millisecond, "client Close must release the public TCP mapping")
}

// TestFallbackPortConflictRollsBackMode proves a real bind conflict during an
// online mode transaction leaves the old HY2 listener and persisted mode intact.
func TestFallbackPortConflictRollsBackMode(t *testing.T) {
	// Hold the TCP side first, then verify the same numeric UDP port is free.
	// The HY2 server can bind UDP while the TCP conflict remains deterministic.
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = blocker.Close() }()
	fixedAddr := blocker.Addr().String()
	pc, err := net.ListenPacket("udp", fixedAddr)
	require.NoError(t, err)
	require.NoError(t, pc.Close())

	es := startE2EServer(t, config.ModeHY2, nextE2EPortRange(), func(cfg *config.ServerConfig) {
		cfg.Listen = fixedAddr
	})
	require.Equal(t, config.ModeHY2, es.srv.Mode())

	err = es.srv.UpdateMode(config.ModeAuto)
	require.Error(t, err, "the TCP side of the dual listener must conflict")
	require.Equal(t, config.ModeHY2, es.srv.Mode(), "a failed bind must not publish the new mode")
	addresses := es.srv.ListenAddresses()
	require.Len(t, addresses, 1)
	require.Contains(t, addresses, transport.HY2)
	require.NotContains(t, addresses, transport.TCP)
	require.NoError(t, blocker.Close())

	require.NoError(t, es.srv.UpdateMode(config.ModeAuto))
	require.Equal(t, config.ModeAuto, es.srv.Mode())
	addresses = es.srv.ListenAddresses()
	require.Len(t, addresses, 2)
	_, udpPort, err := net.SplitHostPort(addresses[transport.HY2])
	require.NoError(t, err)
	_, tcpPort, err := net.SplitHostPort(addresses[transport.TCP])
	require.NoError(t, err)
	require.Equal(t, udpPort, tcpPort, "dual mode must use the same numeric port")

	saved, err := config.LoadServer(es.configPath)
	require.NoError(t, err)
	require.Equal(t, config.ModeAuto, saved.Mode)
}

// probeUnsupportedSession delegates every data-plane operation to the real
// session but reports a protocol error for probes, modelling an old server that
// does not understand the isolated probe channel.
type probeUnsupportedSession struct {
	transport.Session
}

func (s probeUnsupportedSession) Probe(context.Context) (transport.ProbeInfo, error) {
	return transport.ProbeInfo{}, transport.ErrProtocol
}

// TestFallbackProbeUnsupportedKeepsTCPOwner verifies a probe rejection is not a
// reason to abandon the working TCP owner or downgrade its configured mode.
func TestFallbackProbeUnsupportedKeepsTCPOwner(t *testing.T) {
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)
	udpEchoAddr, _ := testutil.StartUDPEcho(t)

	es := startE2EServer(t, config.ModeAuto, nextE2EPortRange())
	gate := newTransportGate(t, es.srv.ListenAddr().String())
	gate.SetUDPEnabled(false)

	cfg := newE2EClientConfig(t, t.TempDir(), "probe-unsupported", gate.Addr(), es.certPath,
		config.ModeAuto, tcpEchoAddr, udpEchoAddr)
	opts := testFailoverOptions()
	opts.Dial = func(ctx context.Context, kind transport.Kind, cfg *config.ClientConfig) (transport.Session, error) {
		session, err := realTestDial(ctx, kind, cfg)
		if err != nil {
			return nil, err
		}
		if kind == transport.HY2 {
			return probeUnsupportedSession{Session: session}, nil
		}
		return session, nil
	}
	ec := startE2EClient(t, cfg, opts)

	ec.waitReady(t, 10*time.Second)
	waitClientTransport(t, ec.cl, "tcp", 10*time.Second)
	serverView := waitServerClient(t, es.srv, cfg.Client.ID, "tcp", 2, 10*time.Second)

	gate.SetUDPEnabled(true)
	require.Eventually(t, func() bool {
		return ec.cl.TransportState().LastProbeError == "protocol"
	}, 5*time.Second, 20*time.Millisecond, "an unsupported probe must be classified as a protocol error")

	state := ec.cl.TransportState()
	require.Equal(t, "tcp", state.ActiveTransport)
	require.Equal(t, uint64(0), state.SwitchCount)
	require.Len(t, es.srv.Clients(), 1)
	require.Equal(t, "tcp", es.srv.Clients()[0].Transport)
	require.NoError(t, tcpEcho(publicAddr(mappingByID(t, serverView.Mappings, "tcp").AssignedPort), "probe-unsupported-tcp"))
	require.Eventually(t, func() bool {
		return udpEcho(publicAddr(mappingByID(t, serverView.Mappings, "udp").AssignedPort), "probe-unsupported-udp") == nil
	}, 5*time.Second, 20*time.Millisecond)

	saved, err := config.LoadClient(cfg.ConfigPath)
	require.NoError(t, err)
	require.Equal(t, config.ModeAuto, saved.Mode)
}

// TestFallbackFinalRegistrationFailureRestoresTCP fails the HY2 candidate's
// final dial after three successful probes. The client must stay on TCP, then
// recover to HY2 once the injected failure is removed.
func TestFallbackFinalRegistrationFailureRestoresTCP(t *testing.T) {
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)
	udpEchoAddr, _ := testutil.StartUDPEcho(t)

	es := startE2EServer(t, config.ModeAuto, nextE2EPortRange())
	gate := newTransportGate(t, es.srv.ListenAddr().String())
	gate.SetUDPEnabled(false)

	cfg := newE2EClientConfig(t, t.TempDir(), "final-registration", gate.Addr(), es.certPath,
		config.ModeAuto, tcpEchoAddr, udpEchoAddr)
	var successfulHY2Dials atomic.Int32
	var armed, failCandidate atomic.Bool
	failCandidate.Store(true)
	opts := testFailoverOptions()
	opts.Dial = func(ctx context.Context, kind transport.Kind, cfg *config.ClientConfig) (transport.Session, error) {
		session, err := realTestDial(ctx, kind, cfg)
		if err != nil {
			return nil, err
		}
		if kind == transport.HY2 && armed.Load() {
			n := successfulHY2Dials.Add(1)
			if n >= 4 && failCandidate.Load() {
				_ = session.Close()
				return nil, &net.OpError{Op: "dial", Net: "udp", Err: syscall.ECONNREFUSED}
			}
		}
		return session, nil
	}
	ec := startE2EClient(t, cfg, opts)

	ec.waitReady(t, 10*time.Second)
	waitClientTransport(t, ec.cl, "tcp", 10*time.Second)
	serverView := waitServerClient(t, es.srv, cfg.Client.ID, "tcp", 2, 10*time.Second)

	// Let the TCP owner age past MinTCPHold while the first probe fails, then
	// allow UDP so three probes succeed and the next HY2 dial is the candidate.
	require.Eventually(t, func() bool {
		st := ec.cl.TransportState()
		return !st.LastProbeAt.IsZero() && st.LastProbeError != ""
	}, 5*time.Second, 20*time.Millisecond)
	armed.Store(true)
	gate.SetUDPEnabled(true)

	require.Eventually(t, func() bool {
		return successfulHY2Dials.Load() >= 4 && ec.cl.TransportState().LastProbeError != ""
	}, 10*time.Second, 20*time.Millisecond, "the final candidate dial must fail")

	state := ec.cl.TransportState()
	require.Equal(t, "tcp", state.ActiveTransport, "a failed final registration must restore/keep TCP")
	require.Equal(t, uint64(0), state.SwitchCount)
	require.Len(t, es.srv.Clients(), 1)
	require.Equal(t, "tcp", es.srv.Clients()[0].Transport)
	require.NoError(t, tcpEcho(publicAddr(mappingByID(t, serverView.Mappings, "tcp").AssignedPort), "tcp-after-candidate-failure"))
	require.Eventually(t, func() bool {
		return udpEcho(publicAddr(mappingByID(t, serverView.Mappings, "udp").AssignedPort), "udp-after-candidate-failure") == nil
	}, 5*time.Second, 20*time.Millisecond)

	saved, err := config.LoadClient(cfg.ConfigPath)
	require.NoError(t, err)
	require.Equal(t, config.ModeAuto, saved.Mode)

	failCandidate.Store(false)
	recovered := waitClientTransport(t, ec.cl, "hy2", 10*time.Second)
	require.Equal(t, "hy2_recovered", recovered.SwitchReason)
	require.Equal(t, uint64(1), recovered.SwitchCount)
	waitServerClient(t, es.srv, cfg.Client.ID, "hy2", 2, 10*time.Second)
	require.NoError(t, tcpEcho(publicAddr(mappingByID(t, serverView.Mappings, "tcp").AssignedPort), "tcp-after-late-recovery"))
}

// realTestDial mirrors the production dial boundary for tests that inject a
// Dial wrapper. It uses only exported transport APIs and the simple non-hopping
// client shape used by these E2E tests.
func realTestDial(ctx context.Context, kind transport.Kind, cfg *config.ClientConfig) (transport.Session, error) {
	opts := transport.DialOptions{
		Auth:         cfg.Auth,
		ControlToken: cfg.Control.Token,
		ClientID:     cfg.Client.ID,
		Up:           uint64(cfg.Bandwidth.Up),
		Down:         uint64(cfg.Bandwidth.Down),
	}
	switch kind {
	case transport.TCP:
		addr, err := cfg.TCPServerAddress()
		if err != nil {
			return nil, err
		}
		tlsConfig, err := transport.ClientTLS(cfg.TLS, addr)
		if err != nil {
			return nil, err
		}
		opts.TCPAddress, opts.TLS = addr, tlsConfig
		return transport.DialTCP(ctx, opts)
	case transport.HY2:
		addr, err := net.ResolveUDPAddr("udp", cfg.Server)
		if err != nil {
			return nil, err
		}
		tlsConfig, err := transport.ClientTLS(cfg.TLS, cfg.Server)
		if err != nil {
			return nil, err
		}
		opts.HY2 = &hyclient.Config{
			ServerAddr: addr,
			Auth:       cfg.Auth,
			TLSConfig: hyclient.TLSConfig{
				ServerName:            tlsConfig.ServerName,
				InsecureSkipVerify:    tlsConfig.InsecureSkipVerify,
				RootCAs:               tlsConfig.RootCAs,
				VerifyPeerCertificate: tlsConfig.VerifyPeerCertificate,
			},
			BandwidthConfig: hyclient.BandwidthConfig{MaxTx: opts.Up, MaxRx: opts.Down},
		}
		return transport.DialHY2(ctx, opts, nil)
	default:
		return nil, transport.ErrProtocol
	}
}

// TestOldVersionCompatibility builds v1.5.4 from the local tag and runs both
// directions of the compatibility matrix in subprocesses. It never replaces the
// production binaries; each fixture lives in a temporary directory.
func TestOldVersionCompatibility(t *testing.T) {
	oldClient, oldServer := buildV154Fixtures(t)

	t.Run("old client to new auto server", func(t *testing.T) {
		testOldClientToNewServer(t, oldClient, config.ModeAuto)
	})
	t.Run("old client to new hy2 server", func(t *testing.T) {
		testOldClientToNewServer(t, oldClient, config.ModeHY2)
	})
	t.Run("new hy2 client to old server", func(t *testing.T) {
		testNewClientToOldServer(t, oldServer)
	})
}

func testOldClientToNewServer(t *testing.T, oldClientBin string, serverMode config.TransportMode) {
	t.Helper()
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)
	udpEchoAddr, _ := testutil.StartUDPEcho(t)

	es := startE2EServer(t, serverMode, nextE2EPortRange())
	dir := t.TempDir()
	cfg := &config.ClientConfig{
		Server:  es.srv.ListenAddr().String(),
		Auth:    e2ePassword,
		TLS:     config.ClientTLSConfig{CA: es.certPath, SNI: "localhost"},
		Control: config.ControlConfig{Token: e2eControlToken},
		Client:  config.ClientIdentity{ID: "v154-old-client", Name: "v154-old-client"},
		Mappings: []config.Mapping{
			{ID: "tcp", Type: "tcp", LocalAddr: tcpEchoAddr, Enabled: true},
			{ID: "udp", Type: "udp", LocalAddr: udpEchoAddr, Enabled: true},
		},
		Web:         config.WebConfig{Listen: ""},
		Log:         config.LogConfig{Level: "info"},
		Credentials: config.CredentialsConfig{Generation: 1},
		ConfigPath:  filepath.Join(dir, "old-client.yaml"),
	}
	require.NoError(t, config.SaveClient(cfg.ConfigPath, cfg))

	proc := startFixtureProcess(t, oldClientBin, "-c", cfg.ConfigPath)
	defer proc.stop()

	serverView := waitServerClient(t, es.srv, cfg.Client.ID, "hy2", 2, 20*time.Second)
	require.NoError(t, tcpEcho(publicAddr(mappingByID(t, serverView.Mappings, "tcp").AssignedPort), "old-client-tcp"))
	require.Eventually(t, func() bool {
		return udpEcho(publicAddr(mappingByID(t, serverView.Mappings, "udp").AssignedPort), "old-client-udp") == nil
	}, 5*time.Second, 20*time.Millisecond)
	require.False(t, proc.exited(), "the v1.5.4 client must stay running")
}

func testNewClientToOldServer(t *testing.T, oldServerBin string) {
	t.Helper()
	tcpEchoAddr, _ := testutil.StartTCPEcho(t)
	udpEchoAddr, _ := testutil.StartUDPEcho(t)

	dir := t.TempDir()
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	certPath, keyPath := writeCertificate(t, dir, cert)
	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	serverAddr := udpConn.LocalAddr().String()
	require.NoError(t, udpConn.Close())

	portRange := nextE2EPortRange()
	serverCfg := &config.ServerConfig{
		Listen:     serverAddr,
		ListenHost: "127.0.0.1",
		Auth:       config.AuthConfig{Type: "password", Password: e2ePassword},
		TLS:        config.TLSConfig{Cert: certPath, Key: keyPath},
		Control:    config.ControlConfig{Token: e2eControlToken},
		Ports:      config.PortsConfig{Allow: []string{portRange}, AutoRange: portRange},
		Web:        config.WebConfig{Listen: ""},
		StateFile:  filepath.Join(dir, "old-server-state.json"),
		Log:        config.LogConfig{Level: "info"},
		ConfigPath: filepath.Join(dir, "old-server.yaml"),
	}
	require.NoError(t, config.SaveServer(serverCfg.ConfigPath, serverCfg))

	proc := startFixtureProcess(t, oldServerBin, "-c", serverCfg.ConfigPath)
	defer proc.stop()
	require.Eventually(t, func() bool {
		if proc.exited() {
			t.Fatalf("the v1.5.4 server exited during startup")
		}
		pc, err := net.ListenPacket("udp", serverAddr)
		if err == nil {
			_ = pc.Close()
			return false
		}
		return true
	}, 10*time.Second, 20*time.Millisecond, "the v1.5.4 server must bind its UDP listener")

	clientCfg := newE2EClientConfig(t, t.TempDir(), "new-client-old-server", serverAddr, certPath,
		config.ModeHY2, tcpEchoAddr, udpEchoAddr)
	ec := startE2EClient(t, clientCfg, testFailoverOptions())
	ec.waitReady(t, 20*time.Second)
	waitClientTransport(t, ec.cl, "hy2", 5*time.Second)
	views := waitClientMappings(t, ec.cl, 2, 10*time.Second)

	require.NoError(t, tcpEcho(publicAddr(mappingByID(t, views, "tcp").AssignedPort), "new-client-old-server-tcp"))
	require.Eventually(t, func() bool {
		return udpEcho(publicAddr(mappingByID(t, views, "udp").AssignedPort), "new-client-old-server-udp") == nil
	}, 5*time.Second, 20*time.Millisecond)
	require.False(t, proc.exited(), "the v1.5.4 server must stay running")
}

// fixtureProcess runs a locally built historical binary and stops it with an
// interrupt before falling back to kill.
type fixtureProcess struct {
	cmd        *exec.Cmd
	done       chan error
	output     bytes.Buffer
	once       sync.Once
	exitedFlag atomic.Bool
}

func startFixtureProcess(t *testing.T, binary string, args ...string) *fixtureProcess {
	t.Helper()
	cmd := exec.Command(binary, args...)
	proc := &fixtureProcess{cmd: cmd, done: make(chan error, 1)}
	cmd.Stdout = &proc.output
	cmd.Stderr = &proc.output
	require.NoError(t, cmd.Start())
	go func() {
		proc.done <- cmd.Wait()
		proc.exitedFlag.Store(true)
	}()
	return proc
}

func (p *fixtureProcess) exited() bool {
	return p.exitedFlag.Load()
}

func (p *fixtureProcess) stop() {
	p.once.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(os.Interrupt)
		}
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			if p.cmd.Process != nil {
				_ = p.cmd.Process.Kill()
			}
			<-p.done
		}
	})
}

// compatUnavailable skips locally when historical fixtures cannot be built,
// but fails when CI explicitly requires the compatibility matrix.
func compatUnavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("HY2TUN_REQUIRE_COMPAT") != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// buildV154Fixtures extracts the local v1.5.4 tag into a temporary directory
// and builds both binaries offline. If the tag or cached modules are missing,
// the compatibility matrix is skipped with an explicit reason rather than
// silently omitted; CI sets HY2TUN_REQUIRE_COMPAT to turn that into a failure.
func buildV154Fixtures(t *testing.T) (clientBin, serverBin string) {
	t.Helper()
	if testing.Short() {
		t.Skip("v1.5.4 compatibility fixtures require building historical binaries")
	}

	rootOut, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		compatUnavailable(t, "v1.5.4 compatibility requires a git checkout: %v", err)
	}
	root := strings.TrimSpace(string(rootOut))
	if err := exec.Command("git", "-C", root, "cat-file", "-e", "v1.5.4^{commit}").Run(); err != nil {
		compatUnavailable(t, "v1.5.4 tag is unavailable (fetch tags to run the compatibility matrix): %v", err)
	}

	dir := t.TempDir()
	archive := exec.Command("git", "-C", root, "archive", "--format=tar", "v1.5.4")
	pipe, err := archive.StdoutPipe()
	if err != nil {
		compatUnavailable(t, "v1.5.4 archive pipe: %v", err)
	}
	tarCmd := exec.Command("tar", "-x", "-C", dir)
	tarCmd.Stdin = pipe
	if err := tarCmd.Start(); err != nil {
		compatUnavailable(t, "v1.5.4 source extraction: %v", err)
	}
	if err := archive.Start(); err != nil {
		_ = tarCmd.Wait()
		compatUnavailable(t, "v1.5.4 archive start: %v", err)
	}
	if err := archive.Wait(); err != nil {
		_ = tarCmd.Wait()
		compatUnavailable(t, "v1.5.4 archive: %v", err)
	}
	if err := tarCmd.Wait(); err != nil {
		compatUnavailable(t, "v1.5.4 source extraction: %v", err)
	}

	build := func(name, pkg string) string {
		out := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-trimpath", "-o", out, pkg)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GOPROXY=off",
			"GOFLAGS=-mod=mod",
			"CGO_ENABLED=0",
			"GOTOOLCHAIN=local",
		)
		if output, err := cmd.CombinedOutput(); err != nil {
			compatUnavailable(t, "cannot build v1.5.4 %s offline: %v\n%s", name, err, tail(output, 2048))
		}
		sum, err := fileSHA256(out)
		require.NoError(t, err)
		t.Logf("v1.5.4 fixture %s sha256=%s", name, sum)
		return out
	}

	clientBin = build("hy2tun-v1.5.4", "./cmd/hy2tun")
	serverBin = build("hy2tund-v1.5.4", "./cmd/hy2tund")
	return clientBin, serverBin
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func tail(data []byte, limit int) string {
	if len(data) <= limit {
		return string(data)
	}
	return string(data[len(data)-limit:])
}
