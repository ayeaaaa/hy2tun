package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/testutil"
)

func testRemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(203, 0, 113, 7), Port: 4242}
}

func decodeLogEntries(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var entries []map[string]any
	dec := json.NewDecoder(buf)
	for {
		var entry map[string]any
		err := dec.Decode(&entry)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		entries = append(entries, entry)
	}
	return entries
}

func TestEventLoggerWritesConnectAndTCPRequest(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	el := newEventLogger(log)
	remote := testRemoteAddr()

	el.Connect(remote, "client-1", 4096)
	el.TCPRequest(remote, "client-1", "example.com:443")

	entries := decodeLogEntries(t, &buf)
	require.Len(t, entries, 2)

	connect := entries[0]
	require.Equal(t, "HY2 client connected", connect["msg"])
	require.Equal(t, remote.String(), connect["remote"])
	require.Equal(t, "client-1", connect["client"])
	require.EqualValues(t, 4096, connect["tx_bytes"])

	request := entries[1]
	require.Equal(t, "HY2 TCP request", request["msg"])
	require.Equal(t, remote.String(), request["remote"])
	require.Equal(t, "client-1", request["client"])
	require.Equal(t, "example.com:443", request["target"])
}

func TestEventLoggerWritesDisconnectAndErrors(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	el := newEventLogger(log)
	remote := testRemoteAddr()

	el.Disconnect(remote, "client-1", errors.New("connection lost"))
	el.TCPError(remote, "client-1", "example.com:443", errors.New("connection refused"))
	el.UDPError(remote, "client-1", 7, errors.New("idle timeout"))

	entries := decodeLogEntries(t, &buf)
	require.Len(t, entries, 3)

	disconnect := entries[0]
	require.Equal(t, "HY2 client disconnected", disconnect["msg"])
	require.Equal(t, "client-1", disconnect["client"])
	require.Equal(t, "connection lost", disconnect["error"])

	tcpError := entries[1]
	require.Equal(t, "HY2 TCP error", tcpError["msg"])
	require.Equal(t, "example.com:443", tcpError["target"])
	require.Equal(t, "connection refused", tcpError["error"])

	udpError := entries[2]
	require.Equal(t, "HY2 UDP error", udpError["msg"])
	require.EqualValues(t, 7, udpError["session"])
	require.Equal(t, "idle timeout", udpError["error"])
}

func TestEventLoggerRedactsVirtualTokens(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	el := newEventLogger(log)
	remote := testRemoteAddr()
	const token = "super-secret-token"

	el.TCPRequest(remote, "client-1", "_hy2tun_data_"+token+":0")
	el.UDPRequest(remote, "client-1", 7, "_hy2tun_udp_"+token+":0")
	el.TCPRequest(remote, "client-1", "_hy2tun_"+token+":0")

	out := buf.String()
	require.NotContains(t, out, token)
	require.Contains(t, out, "_hy2tun_data_[redacted]:0")
	require.Contains(t, out, "_hy2tun_udp_[redacted]:0")
	require.Contains(t, out, "_hy2tun_[redacted]:0")
}

func TestEventLoggerIsWiredIntoServer(t *testing.T) {
	echoAddr, stopEcho := testutil.StartTCPEcho(t)
	defer stopEcho()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cert, err := testutil.SelfSignedCert("localhost")
	require.NoError(t, err)
	cfg := validServerConfig(cert)
	cfg.Auth.Password = "server-password-sentinel"
	cfg.Control.Token = "control-token-sentinel-0123456789"

	srv, err := New(cfg, log)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	defer srv.Close()

	cl := newHY2Client(t, srv, "server-password-sentinel", nil)
	conn, err := cl.TCP(echoAddr)
	require.NoError(t, err)
	defer conn.Close()
	requireTCPEcho(t, conn, "event")

	out := buf.String()
	require.Contains(t, out, `"msg":"HY2 client connected"`)
	require.Contains(t, out, `"msg":"HY2 TCP request"`)
	require.Contains(t, out, `"client":"user"`)
	require.Contains(t, out, echoAddr)
	require.NotContains(t, out, "server-password-sentinel")
	require.NotContains(t, out, "control-token-sentinel")
}
