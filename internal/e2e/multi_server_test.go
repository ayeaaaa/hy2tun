package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/client"
	"hy2tun/internal/config"
	"hy2tun/internal/server"
	"hy2tun/internal/testutil"
)

func startMultiServer(t *testing.T, dir, name, certPath, keyPath string) (*server.Server, func()) {
	t.Helper()
	cfg := &config.ServerConfig{
		Listen:  "127.0.0.1:0",
		Auth:    config.AuthConfig{Type: "password", Password: e2ePassword},
		TLS:     config.TLSConfig{Cert: certPath, Key: keyPath},
		Control: config.ControlConfig{Token: e2eControlToken},
		Ports: config.PortsConfig{
			Allow:     []string{"32000-32999"},
			AutoRange: "32000-32999",
		},
		Web:       config.WebConfig{Listen: ""},
		StateFile: filepath.Join(dir, name+"-state.json"),
		Log:       config.LogConfig{Level: "info"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := server.New(cfg, logger)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Start(ctx) }()
	stop := func() {
		cancel()
		_ = srv.Close()
	}
	t.Cleanup(stop)
	return srv, stop
}

func freeE2EAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func TestMultiServerIndependentMappingsAndProxy(t *testing.T) {
	echoAddr, _ := testutil.StartTCPEcho(t)
	dir := t.TempDir()
	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	certPath, keyPath := writeCertificate(t, dir, cert)

	us, stopUS := startMultiServer(t, dir, "us", certPath, keyPath)
	sg, _ := startMultiServer(t, dir, "sg", certPath, keyPath)

	clientCfg := &config.ClientConfig{
		Web: config.WebConfig{Listen: ""},
		Log: config.LogConfig{Level: "info"},
		Servers: []config.ClientServerConfig{
			{
				Name: "us", Server: us.ListenAddr().String(), Auth: e2ePassword,
				TLS:      config.ClientTLSConfig{CA: certPath},
				Control:  config.ControlConfig{Token: e2eControlToken},
				Client:   config.ClientIdentity{ID: "multi-us", Name: "multi-us"},
				Mappings: []config.Mapping{{ID: "tcp", Type: "tcp", LocalAddr: echoAddr, Enabled: true}},
			},
			{
				Name: "sg", Server: sg.ListenAddr().String(), Auth: e2ePassword,
				TLS:      config.ClientTLSConfig{CA: certPath},
				Control:  config.ControlConfig{Token: e2eControlToken},
				Client:   config.ClientIdentity{ID: "multi-sg", Name: "multi-sg"},
				Mappings: []config.Mapping{{ID: "tcp", Type: "tcp", LocalAddr: echoAddr, Enabled: true}},
			},
		},
	}
	clientCfg.ConfigPath = filepath.Join(dir, "client.yaml")
	require.NoError(t, config.SaveClient(clientCfg.ConfigPath, clientCfg))
	loaded, err := config.LoadClient(clientCfg.ConfigPath)
	require.NoError(t, err)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup, err := client.NewSupervisor(loaded, logger)
	require.NoError(t, err)
	defer sup.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sup.Start(ctx) }()

	var usPort, sgPort int
	require.Eventually(t, func() bool {
		usView := us.Clients()
		sgView := sg.Clients()
		if len(usView) != 1 || len(sgView) != 1 {
			return false
		}
		if len(usView[0].Mappings) != 1 || len(sgView[0].Mappings) != 1 {
			return false
		}
		if usView[0].Mappings[0].Status != "running" || sgView[0].Mappings[0].Status != "running" {
			return false
		}
		usPort = usView[0].Mappings[0].AssignedPort
		sgPort = sgView[0].Mappings[0].AssignedPort
		return usPort != 0 && sgPort != 0 && usPort != sgPort
	}, 15*time.Second, 50*time.Millisecond, "both independent mappings must be running")

	require.NoError(t, tcpEcho(fmt.Sprintf("127.0.0.1:%d", usPort), "multi-us"))
	require.NoError(t, tcpEcho(fmt.Sprintf("127.0.0.1:%d", sgPort), "multi-sg"))

	proxyAddr := freeE2EAddress(t)
	require.NoError(t, sup.UpdateProxy("us", config.ProxyConfig{Enabled: true, SOCKS5: proxyAddr}))
	occupied, err := net.Listen("tcp", proxyAddr)
	require.Error(t, err, "enabled proxy must own its port")
	if occupied != nil {
		_ = occupied.Close()
	}
	require.NoError(t, sup.UpdateProxy("us", config.ProxyConfig{Enabled: false, SOCKS5: proxyAddr}))
	released, err := net.Listen("tcp", proxyAddr)
	require.NoError(t, err, "disabled proxy must release its port")
	_ = released.Close()

	// Stopping the US server must not stop the SG mapping.
	stopUS()
	require.Eventually(t, func() bool {
		return tcpEcho(fmt.Sprintf("127.0.0.1:%d", sgPort), "sg-survives-us-stop") == nil
	}, 10*time.Second, 50*time.Millisecond, "SG mapping must survive the US server stopping")
}
