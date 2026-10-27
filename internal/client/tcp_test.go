package client

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPipeCopiesBothWaysAndClosesBoth(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()

	local, localPeer := net.Pipe()
	remote, remotePeer := net.Pipe()
	defer localPeer.Close()
	defer remotePeer.Close()

	done := make(chan struct{})
	go func() {
		cl.pipe(local, remote)
		close(done)
	}()

	go func() { _, _ = localPeer.Write([]byte("ping")) }()
	buf := make([]byte, 4)
	_, err = io.ReadFull(remotePeer, buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf))

	go func() { _, _ = remotePeer.Write([]byte("pong")) }()
	_, err = io.ReadFull(localPeer, buf)
	require.NoError(t, err)
	require.Equal(t, "pong", string(buf))

	require.NoError(t, remotePeer.Close())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipe did not stop after one side closed")
	}

	_ = localPeer.SetReadDeadline(time.Now().Add(time.Second))
	_, err = localPeer.Read(make([]byte, 1))
	require.Error(t, err, "the other side must be closed too")
}

func TestValidDataToken(t *testing.T) {
	require.True(t, validDataToken(strings.Repeat("a", 32)))
	require.True(t, validDataToken("0123456789abcdef0123456789abcdef"))
	require.False(t, validDataToken(""))
	require.False(t, validDataToken(strings.Repeat("a", 31)))
	require.False(t, validDataToken(strings.Repeat("z", 32)))
}
