package transport

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTCPPeerWithLiveSerializesCloseAndAdmission(t *testing.T) {
	_, peer, _ := tcpPair(t, RateLimits{}, 0, 0)
	entered, release, admitted, closed := make(chan struct{}), make(chan struct{}), make(chan error, 1), make(chan error, 1)
	var releaseOnce sync.Once
	resume := func() { releaseOnce.Do(func() { close(release) }) }
	defer resume()
	go func() { admitted <- peer.WithLive(func() error { close(entered); <-release; return nil }) }()
	<-entered
	go func() { closed <- peer.Close() }()
	select {
	case <-closed:
		t.Fatal("Close completed during admission commit")
	case <-time.After(20 * time.Millisecond):
	}
	// The callback and closure must have a single, observable order.
	resume()
	require.NoError(t, <-admitted)
	require.NoError(t, <-closed)
	called := false
	err := peer.WithLive(func() error { called = true; return nil })
	require.ErrorIs(t, err, net.ErrClosed)
	require.False(t, called, "closed root cannot execute admission")
}

func TestTCPPeerWithLiveRejectsRemoteClosedRoot(t *testing.T) {
	root, peer, _ := tcpPair(t, RateLimits{}, 0, 0)
	require.NoError(t, root.Close())
	require.Eventually(t, func() bool { return peer.session.mux.IsClosed() }, time.Second, time.Millisecond)
	called := false
	err := peer.WithLive(func() error { called = true; return nil })
	require.Error(t, err)
	require.False(t, called)
}
