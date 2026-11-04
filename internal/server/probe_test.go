package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/transport"
	"hy2tun/internal/version"
)

func controlPingForTest(c net.Conn) error {
	if err := control.WriteMessage(c, &control.Ping{Type: control.TypePing, TS: 123}); err != nil {
		return err
	}
	_, err := control.ReadMessage(c)
	return err
}

func TestProbeDoesNotRegisterOrPersist(t *testing.T) {
	dir := t.TempDir() // Must outlive the server's persistent-state cleanup.
	s, tlsConfig := tcpTestServer(t)
	// Use real persistence and an existing live mapping: probe must not replace it.
	s.currentConfig().StateFile = filepath.Join(dir, "state.json")
	require.NoError(t, s.store.Close())
	require.NoError(t, s.initState())
	s.mappings.store = s.store
	root := tcpTestDial(t, tcpTestEndpoint(t, s, tlsConfig), "owner", "pw", s.currentConfig().Control.Token)
	c, _ := tcpTestRegister(t, root, "owner", s.currentConfig().Control.Token, config.Mapping{ID: "tcp", Type: "tcp", LocalAddr: "127.0.0.1:1", Enabled: true})
	owner := liveTCPTestSession(t, s, "owner")
	before := s.Clients()
	stateBefore, err := os.ReadFile(s.currentConfig().StateFile)
	require.NoError(t, err)
	s.mappings.mu.Lock()
	listener := owner.mappings["tcp"].tcpListener
	s.mappings.mu.Unlock()
	for i := 0; i < 3; i++ {
		probe, err := (&outbound{server: s}).TCP("_hy2tun_probe_:0")
		require.NoError(t, err)
		require.NoError(t, probe.SetDeadline(time.Now().Add(time.Second)))
		nonce := "11223344556677889900aabbccddeeff"
		require.NoError(t, transport.WriteProbeRequest(probe, transport.ProbeRequest{Version: 1, Token: s.currentConfig().Control.Token, Nonce: nonce}))
		reply, err := transport.ReadProbeReply(probe)
		require.NoError(t, err)
		require.True(t, reply.OK)
		require.Equal(t, transport.ProbeInfo{Version: 1, Nonce: nonce, Mode: config.ModeAuto, ServerVersion: version.Version}, reply.Info)
		require.NoError(t, probe.Close())
	}
	require.Equal(t, before, s.Clients())
	stateAfter, err := os.ReadFile(s.currentConfig().StateFile)
	require.NoError(t, err)
	require.True(t, bytes.Equal(stateBefore, stateAfter))
	s.mappings.mu.Lock()
	require.Same(t, listener, owner.mappings["tcp"].tcpListener)
	s.mappings.mu.Unlock()
	require.NoError(t, c.SetDeadline(time.Now().Add(time.Second)))
	require.NoError(t, controlPingForTest(c))
}

func TestProbeBoundedAndUnauthorized(t *testing.T) {
	s, _ := tcpTestServer(t)
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"oversize", binary.BigEndian.AppendUint32(nil, 65537)},
		{"unauthorized", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := net.Pipe()
			go s.serveProbe(a)
			defer b.Close()
			require.NoError(t, b.SetDeadline(time.Now().Add(time.Second)))
			if tc.body != nil {
				_, err := b.Write(tc.body)
				require.NoError(t, err)
			} else {
				require.NoError(t, transport.WriteProbeRequest(b, transport.ProbeRequest{Version: 1, Token: "wrong-secret", Nonce: "11223344556677889900aabbccddeeff"}))
			}
			reply, err := transport.ReadProbeReply(b)
			require.NoError(t, err)
			require.False(t, reply.OK)
			require.Empty(t, reply.Info)
			if tc.body != nil {
				require.Equal(t, "bad_message", reply.Error)
			} else {
				require.Equal(t, "unauthorized", reply.Error)
			}
		})
	}
	require.Empty(t, s.Clients())
}

func TestProbeConcurrencyBound(t *testing.T) {
	s, _ := tcpTestServer(t)
	var children []net.Conn
	defer func() {
		for _, c := range children {
			_ = c.Close()
		}
	}()
	for i := 0; i < 32; i++ {
		a, b := net.Pipe()
		children = append(children, b)
		go s.serveProbe(a)
	}
	require.Eventually(t, func() bool { return len(s.probeSlots) == 32 }, time.Second, time.Millisecond)
	a, b := net.Pipe()
	defer b.Close()
	go s.serveProbe(a)
	require.NoError(t, b.SetReadDeadline(time.Now().Add(time.Second)))
	_, err := b.Read(make([]byte, 1))
	require.Error(t, err)
	require.False(t, isTimeout(err), "33rd simultaneous probe must be rejected immediately")
	for _, c := range children {
		_ = c.Close()
	}
	require.Eventually(t, func() bool { return len(s.probeSlots) == 0 }, time.Second, time.Millisecond)
}

func TestProbeHY2AdapterDoesNotRegister(t *testing.T) {
	s, stop := startTestServer(t)
	defer stop()
	root := transport.WrapHY2(newHY2Client(t, s, "pw", nil), s.currentConfig().Control.Token)
	defer root.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	info, err := root.Probe(ctx)
	require.NoError(t, err)
	require.Equal(t, config.ModeHY2, info.Mode)
	require.Len(t, info.Nonce, 32)
	require.Empty(t, s.Clients())
}
