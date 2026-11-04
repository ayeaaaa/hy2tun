package transport

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fatedier/yamux"
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

// Delay physical teardown only after the real mux has published shutdown.
type admissionCloseGate struct {
	net.Conn
	entered, release chan struct{}
	once             sync.Once
}

func (c *admissionCloseGate) Close() error {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Conn.Close()
}

func TestTCPPeerWithLiveRejectsQueuedRemoteShutdownWithoutDrain(t *testing.T) {
	a, b := net.Pipe()
	gate := &admissionCloseGate{Conn: b, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	defer release()
	cfg := tcpMuxConfig()
	cfg.EnableKeepAlive = false
	cfg.StreamOpenTimeout = 0
	serverMux, err := yamux.Server(gate, cfg)
	require.NoError(t, err)
	clientMux, err := yamux.Client(a, cfg)
	require.NoError(t, err)
	peer := &TCPPeer{session: newTCPSession(serverMux, 0, 0)}
	defer func() { release(); _ = clientMux.Close(); _ = peer.Close() }()
	_, err = clientMux.OpenStream()
	require.NoError(t, err)
	require.Eventually(t, func() bool { return serverMux.NumStreams() == 1 }, time.Second, time.Millisecond)
	require.NoError(t, clientMux.Close())
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("remote shutdown did not enter physical close")
	}
	require.True(t, serverMux.IsClosed())
	var callback atomic.Bool
	// Before the fix AcceptStream randomly chooses queued work or shutdown.
	// Repeated rejections exercise the queued branch without relying on one select.
	for attempt := 0; attempt < 32; attempt++ {
		done := make(chan error, 1)
		go func() { done <- peer.WithLive(func() error { callback.Store(true); return nil }) }()
		select {
		case err := <-done:
			if !errors.Is(err, net.ErrClosed) {
				t.Errorf("admission rejection classified remote I/O instead of returning closed sentinel: %v", err)
			}
		case <-time.After(100 * time.Millisecond):
			t.Error("WithLive waited for queued-stream teardown instead of rejecting immediately")
			release()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("admission did not recover after close gate released")
			}
			require.False(t, callback.Load())
			return
		}
	}
	require.False(t, callback.Load())
}
