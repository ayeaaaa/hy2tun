package client

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"
	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/transport"
)

type blockedFactoryHY2 struct {
	*gatedDataHY2
	entered, release chan struct{}
	closeOnce        sync.Once
}

// Probe teardown has several owned close paths. Keep each wait channel-based
// so synctest can observe the blocked cleanup without a sync.Once mutex wait.
type probeCleanupConn struct {
	net.Conn
	entered, release chan struct{}
	started          atomic.Bool
}

func (c *probeCleanupConn) Close() error {
	if c.started.CompareAndSwap(false, true) {
		close(c.entered)
	}
	<-c.release
	return c.Conn.Close()
}

func (f *blockedFactoryHY2) Close() error {
	f.closeOnce.Do(func() { close(f.entered); <-f.release; _ = f.gatedDataHY2.Close() })
	return nil
}

func TestHY2NativeDialReservationRetainsFactoryAndCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl, err := New(validTestClientConfig(), nil)
		require.NoError(t, err)
		defer cl.Close()
		nativeRelease := make(chan struct{})
		fake := &blockedFactoryHY2{gatedDataHY2: &gatedDataHY2{closed: make(chan struct{})}, entered: make(chan struct{}), release: make(chan struct{})}
		var nativeOnce, cleanupOnce sync.Once
		defer nativeOnce.Do(func() { close(nativeRelease) })
		defer cleanupOnce.Do(func() { close(fake.release) })
		var calls atomic.Int32
		stop := errors.New("second factory was admitted")
		cl.newHY2 = func(*hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error) {
			if calls.Add(1) != 1 {
				return nil, nil, stop
			}
			<-nativeRelease
			return fake, nil, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := cl.dialSession(ctx, transport.HY2, cl.snapshotConfig()); done <- err }()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		_, err = cl.dialSession(context.Background(), transport.HY2, cl.snapshotConfig())
		require.ErrorIs(t, err, transport.ErrQueueFull, "cancelled outer dial is not native factory completion")
		require.Equal(t, int32(1), calls.Load())
		nativeOnce.Do(func() { close(nativeRelease) })
		<-fake.entered
		_, err = cl.dialSession(context.Background(), transport.HY2, cl.snapshotConfig())
		require.ErrorIs(t, err, transport.ErrQueueFull, "native return is not abandoned-root cleanup completion")
		cleanupOnce.Do(func() { close(fake.release) })
		synctest.Wait()
		_, err = cl.dialSession(context.Background(), transport.HY2, cl.snapshotConfig())
		require.ErrorIs(t, err, stop)
		require.Equal(t, int32(2), calls.Load())
	})
}

func TestHY2ProbeReservationRetainsNativeAndLateCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := validTestClientConfig()
		cfg.Mode = config.ModeAuto
		clock := newPolicyClock()
		h := &policyHarness{hellos: make(chan *control.Hello, 20)}
		nativeRelease := make(chan struct{})
		local, peer := net.Pipe()
		defer peer.Close()
		child := &probeCleanupConn{Conn: local, entered: make(chan struct{}), release: make(chan struct{})}
		var nativeOnce, cleanupOnce sync.Once
		defer nativeOnce.Do(func() { close(nativeRelease) })
		defer cleanupOnce.Do(func() { close(child.release) })
		var calls atomic.Int32
		dials := 0
		cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Clock: clock, Policy: recoveryTestPolicy(), Jitter: func(d time.Duration) time.Duration { return d }, Dial: func(_ context.Context, kind transport.Kind, _ *config.ClientConfig) (transport.Session, error) {
			dials++
			if dials == 1 {
				return nil, syscall.ECONNREFUSED
			}
			if kind == transport.TCP {
				s := newSessionStub(kind)
				s.control = h.control(kind)
				return s, nil
			}
			f := &gatedDataHY2{closed: make(chan struct{})}
			f.tcp = func(addr string) (net.Conn, error) {
				if addr == "_hy2tun_control_:0" {
					return h.control(transport.HY2), nil
				}
				if addr != "_hy2tun_probe_:0" {
					return nil, transport.ErrProtocol
				}
				if calls.Add(1) != 1 {
					return nil, io.EOF
				}
				<-nativeRelease
				return child, nil
			}
			return transport.WrapHY2(f, cfg.Control.Token), nil
		}})
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { done <- cl.Start(context.Background()) }()
		synctest.Wait()
		defer func() {
			cl.Close()
			<-done
			nativeOnce.Do(func() { close(nativeRelease) })
			cleanupOnce.Do(func() { close(child.release) })
			synctest.Wait()
		}()
		for range 5 {
			clock.advance(10 * time.Second)
		}
		require.Equal(t, int32(1), calls.Load(), "new roots must share the outstanding native-probe reservation")
		require.Equal(t, "tcp", cl.TransportState().ActiveTransport)
		nativeOnce.Do(func() { close(nativeRelease) })
		<-child.entered
		for range 2 {
			clock.advance(10 * time.Second)
		}
		require.Equal(t, int32(1), calls.Load(), "blocked late cleanup must still consume probe admission")
		cl.forceReconnect()
		synctest.Wait()
		require.Equal(t, "hy2", cl.TransportState().ActiveTransport, "probe backpressure must not hide data/control completion or prohibit a new owner")
		a := cl.newAttempt(context.Background(), transport.HY2, "probe")
		err = cl.probeAttempt(a)
		cl.discardAttempt(a)
		require.ErrorIs(t, err, transport.ErrQueueFull)
		require.Equal(t, int32(1), calls.Load())
		cleanupOnce.Do(func() { close(child.release) })
		synctest.Wait()
		a = cl.newAttempt(context.Background(), transport.HY2, "probe")
		err = cl.probeAttempt(a)
		cl.discardAttempt(a)
		require.ErrorIs(t, err, io.EOF)
		require.Equal(t, int32(2), calls.Load())
	})
}

func TestHY2ControlReservationRetainsNativeAcrossReplacement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl, err := New(validTestClientConfig(), nil)
		require.NoError(t, err)
		defer cl.Close()
		nativeRelease := make(chan struct{})
		local, peer := net.Pipe()
		defer peer.Close()
		child := &blockedChildClose{Conn: local, entered: make(chan struct{}), release: make(chan struct{})}
		var nativeOnce, cleanupOnce sync.Once
		defer nativeOnce.Do(func() { close(nativeRelease) })
		defer cleanupOnce.Do(func() { close(child.release) })
		var calls atomic.Int32
		makeSession := func() transport.Session {
			f := &gatedDataHY2{closed: make(chan struct{}), tcp: func(string) (net.Conn, error) {
				if calls.Add(1) != 1 {
					return nil, io.EOF
				}
				<-nativeRelease
				return child, nil
			}}
			return transport.WrapHY2(f, cl.cfg.Control.Token)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := cl.runSessionWithSetup(context.Background(), ctx, makeSession(), 1); done <- err }()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		_, err = cl.runSessionWithSetup(context.Background(), context.Background(), makeSession(), 1)
		require.ErrorIs(t, err, transport.ErrQueueFull)
		require.Equal(t, int32(1), calls.Load())
		nativeOnce.Do(func() { close(nativeRelease) })
		<-child.entered
		_, err = cl.runSessionWithSetup(context.Background(), context.Background(), makeSession(), 1)
		require.ErrorIs(t, err, transport.ErrQueueFull)
		cleanupOnce.Do(func() { close(child.release) })
		synctest.Wait()
		_, err = cl.runSessionWithSetup(context.Background(), context.Background(), makeSession(), 1)
		require.ErrorIs(t, err, io.EOF)
		require.Equal(t, int32(2), calls.Load())
	})
}

func TestAutoPendingHY2DialDoesNotStarveTCPReconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := validTestClientConfig()
		cfg.Mode = config.ModeAuto
		clock := newPolicyClock()
		release := make(chan struct{})
		h := &policyHarness{hellos: make(chan *control.Hello, 10)}
		var hy2Calls, tcpCalls atomic.Int32
		cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Clock: clock, Jitter: func(d time.Duration) time.Duration { return d }, Dial: func(_ context.Context, kind transport.Kind, _ *config.ClientConfig) (transport.Session, error) {
			if kind == transport.HY2 {
				hy2Calls.Add(1)
				<-release
				return nil, io.EOF
			}
			tcpCalls.Add(1)
			s := newSessionStub(kind)
			s.control = h.control(kind)
			return s, nil
		}})
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { done <- cl.Start(context.Background()) }()
		synctest.Wait()
		defer func() { cl.Close(); <-done; close(release); synctest.Wait() }()
		clock.advance(10 * time.Second)
		require.Equal(t, "tcp", cl.TransportState().ActiveTransport)
		closeLiveConnection(nil, cl.currentSession())
		synctest.Wait()
		clock.advance(time.Second)
		require.Equal(t, int32(1), hy2Calls.Load(), "pending native HY2 dial lost its reservation")
		require.Equal(t, int32(2), tcpCalls.Load(), "local HY2 admission backpressure starved recovery of the already-eligible TCP route")
		require.Equal(t, "tcp", cl.TransportState().ActiveTransport)
	})
}

func TestAutoSetupBackpressureCannotHideSecurityFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := validTestClientConfig()
		cfg.Mode = config.ModeAuto
		clock := newPolicyClock()
		h := &policyHarness{hellos: make(chan *control.Hello, 10)}
		var kinds []transport.Kind
		cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Clock: clock, Jitter: func(d time.Duration) time.Duration { return d }, Dial: func(_ context.Context, kind transport.Kind, _ *config.ClientConfig) (transport.Session, error) {
			kinds = append(kinds, kind)
			switch len(kinds) {
			case 1:
				return nil, syscall.ECONNREFUSED
			case 2:
				s := newSessionStub(kind)
				s.control = h.control(kind)
				return s, nil
			case 3:
				return nil, errors.Join(errSetupBusy, transport.ErrAuthentication)
			default:
				return nil, errSetupBusy
			}
		}})
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { done <- cl.Start(context.Background()) }()
		synctest.Wait()
		defer func() { cl.Close(); <-done; synctest.Wait() }()
		closeLiveConnection(nil, cl.currentSession())
		synctest.Wait()
		clock.advance(time.Second)
		clock.advance(2 * time.Second)
		require.Equal(t, []transport.Kind{transport.HY2, transport.TCP, transport.HY2, transport.HY2}, kinds)
		require.Empty(t, cl.TransportState().ActiveTransport, "security rejection must withdraw old network fallback permission")
	})
}
