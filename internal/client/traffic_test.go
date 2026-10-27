package client

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
)

func TestPipeCountsPerMappingTraffic(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Mappings = []config.Mapping{{
		ID:        "m1",
		Type:      "tcp",
		LocalAddr: "127.0.0.1:22",
		Enabled:   true,
	}}
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer cl.Close()

	local, localPeer := net.Pipe()
	remote, remotePeer := net.Pipe()
	defer localPeer.Close()
	defer remotePeer.Close()

	done := make(chan struct{})
	go func() {
		cl.pipeMapping("m1", local, remote)
		close(done)
	}()

	go func() { _, _ = localPeer.Write([]byte("ping")) }()
	buf := make([]byte, 4)
	_, err = io.ReadFull(remotePeer, buf)
	require.NoError(t, err)

	go func() { _, _ = remotePeer.Write([]byte("pong")) }()
	_, err = io.ReadFull(localPeer, buf)
	require.NoError(t, err)

	require.NoError(t, remotePeer.Close())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipe did not stop after one side closed")
	}

	require.Eventually(t, func() bool {
		mappings := cl.Mappings()
		return len(mappings) == 1 && mappings[0].TxBytes == 4 && mappings[0].RxBytes == 4
	}, time.Second, 10*time.Millisecond)

	status := cl.Status()
	require.Equal(t, uint64(4), status.TxBytes)
	require.Equal(t, uint64(4), status.RxBytes)
}
