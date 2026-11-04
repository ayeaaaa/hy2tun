package client

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/transport"
)

// Later Close invocations return even while the first owns blocked cleanup.
// Keep the real HY2 Probe and real client reservation/controller underneath.
type pendingProbeCallbackClose struct {
	net.Conn
	started                    atomic.Bool
	entered, release, finished chan struct{}
}

func (c *pendingProbeCallbackClose) Close() error {
	if c.started.CompareAndSwap(false, true) {
		_ = c.Conn.Close()
		close(c.entered)
		<-c.release
		close(c.finished)
	}
	return nil
}

func TestHY2ProbeReservationJoinsCallbackCleanupAcrossRoots(t *testing.T) {
	for _, shutdown := range []string{"cancel", "root"} {
		t.Run(shutdown, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cl, err := New(validTestClientConfig(), nil)
				require.NoError(t, err)
				defer cl.Close()
				local, peer := net.Pipe()
				defer peer.Close()
				child := &pendingProbeCallbackClose{Conn: local, entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
				var releaseOnce sync.Once
				defer func() { releaseOnce.Do(func() { close(child.release) }); <-child.finished; synctest.Wait() }()
				first := &gatedDataHY2{closed: make(chan struct{}), tcp: func(string) (net.Conn, error) { return child, nil }}
				s := transport.WrapHY2(first, cl.cfg.Control.Token)
				defer s.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- cl.probeSetup(ctx, s) }()
				_, err = transport.ReadProbeRequest(peer)
				require.NoError(t, err)
				if shutdown == "cancel" {
					cancel()
				} else {
					require.NoError(t, s.Close())
				}
				require.Error(t, <-done)
				<-child.entered
				synctest.Wait()
				var calls atomic.Int32
				second := &gatedDataHY2{closed: make(chan struct{}), tcp: func(string) (net.Conn, error) { calls.Add(1); return nil, io.EOF }}
				replacement := transport.WrapHY2(second, cl.cfg.Control.Token)
				defer replacement.Close()
				err = cl.probeSetup(context.Background(), replacement)
				require.ErrorIs(t, err, transport.ErrQueueFull, "known callback cleanup must retain the same worker reservation")
				require.Zero(t, calls.Load())
				releaseOnce.Do(func() { close(child.release) })
				<-child.finished
				synctest.Wait()
				err = cl.probeSetup(context.Background(), replacement)
				require.ErrorIs(t, err, io.EOF)
				require.Equal(t, int32(1), calls.Load())
			})
		})
	}
}

func TestAutoProbeCallbackCleanupCannotOverlapReplacementNativeProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := validTestClientConfig()
		cfg.Mode = config.ModeAuto
		clock := newPolicyClock()
		h := &policyHarness{hellos: make(chan *control.Hello, 10)}
		local, peer := net.Pipe()
		defer peer.Close()
		child := &pendingProbeCallbackClose{Conn: local, entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
		var releaseOnce sync.Once
		var nativeCalls, dials atomic.Int32
		cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Clock: clock, Policy: recoveryTestPolicy(), Jitter: func(d time.Duration) time.Duration { return d }, Dial: func(_ context.Context, kind transport.Kind, snapshot *config.ClientConfig) (transport.Session, error) {
			if dials.Add(1) == 1 {
				return nil, syscall.ECONNREFUSED
			}
			if kind == transport.TCP {
				s := newSessionStub(kind)
				s.control = h.control(kind)
				return s, nil
			}
			f := &gatedDataHY2{closed: make(chan struct{}), tcp: func(addr string) (net.Conn, error) {
				if addr != "_hy2tun_probe_:0" {
					return nil, transport.ErrProtocol
				}
				if nativeCalls.Add(1) == 1 {
					return child, nil
				}
				return nil, io.EOF
			}}
			return transport.WrapHY2(f, snapshot.Control.Token), nil
		}})
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { done <- cl.Start(context.Background()) }()
		synctest.Wait()
		defer func() {
			cl.Close()
			<-done
			releaseOnce.Do(func() { close(child.release) })
			<-child.finished
			synctest.Wait()
		}()
		clock.advance(10 * time.Second)
		_, err = transport.ReadProbeRequest(peer)
		require.NoError(t, err)
		synctest.Wait()
		clock.advance(10 * time.Second)
		<-child.entered // 20s timeout starts asymmetric callback cleanup.
		for range 3 {
			clock.advance(10 * time.Second)
		}
		require.Equal(t, "tcp", cl.TransportState().ActiveTransport)
		require.True(t, cl.Status().Connected)
		require.Equal(t, int32(1), nativeCalls.Load(), "new policy rounds overlapped old callback cleanup")
		releaseOnce.Do(func() { close(child.release) })
		<-child.finished
		synctest.Wait()
		clock.advance(10 * time.Second)
		require.Equal(t, int32(2), nativeCalls.Load(), "actual cleanup completion must release probe admission")
		require.Equal(t, "tcp", cl.TransportState().ActiveTransport)
	})
}
