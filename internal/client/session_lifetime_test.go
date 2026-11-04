package client

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/transport"
)

// Keep the real WrapHY2 cancellation/registration behavior. Only the native
// synchronous calls are gated, just as they can stall on network I/O.
type gatedDataHY2 struct {
	tcp    func(string) (net.Conn, error)
	udp    func() (hyclient.HyUDPConn, error)
	closed chan struct{}
	once   sync.Once
}

func (f *gatedDataHY2) TCP(addr string) (net.Conn, error) { return f.tcp(addr) }
func (f *gatedDataHY2) UDP() (hyclient.HyUDPConn, error)  { return f.udp() }
func (f *gatedDataHY2) Close() error                      { f.once.Do(func() { close(f.closed) }); return nil }
func (f *gatedDataHY2) isClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}

func installGatedHY2(t *testing.T, f *gatedDataHY2) (*Client, net.Conn) {
	t.Helper()
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	owner, peer := net.Pipe()
	require.True(t, cl.setCurrent(transport.WrapHY2(f, cl.cfg.Control.Token), owner))
	cl.markConnected()
	t.Cleanup(func() { _ = peer.Close(); _ = cl.Close() })
	return cl, peer
}

type closeObservedConn struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (c *closeObservedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func TestHY2ProxyRequestCancellationPreservesOwner(t *testing.T) {
	for _, reason := range []string{"cancel", "deadline"} {
		t.Run(reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started, release := make(chan struct{}), make(chan struct{})
				live, livePeer := net.Pipe()
				defer livePeer.Close()
				late, latePeer := net.Pipe()
				defer latePeer.Close()
				observed := &closeObservedConn{Conn: late, closed: make(chan struct{})}
				f := &gatedDataHY2{closed: make(chan struct{})}
				f.tcp = func(addr string) (net.Conn, error) {
					if addr == "live.example:443" {
						return live, nil
					}
					close(started)
					select {
					case <-release:
						return observed, nil
					case <-f.closed:
						return nil, net.ErrClosed
					}
				}
				cl, _ := installGatedHY2(t, f)
				unrelated, err := cl.TCP("live.example:443")
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(context.Background())
				want := context.Canceled
				if reason == "deadline" {
					ctx, cancel = context.WithTimeout(context.Background(), time.Second)
					want = context.DeadlineExceeded
				}
				defer cancel()
				done := make(chan error, 1)
				go func() {
					conn, err := cl.TCPContext(ctx, "pending.example:443")
					if conn != nil {
						_ = conn.Close()
					}
					done <- err
				}()
				<-started
				if reason == "cancel" {
					cancel()
				}
				require.ErrorIs(t, <-done, want)
				require.False(t, f.isClosed(), "one request must not close its established HY2 owner")
				go func() { _, _ = livePeer.Write([]byte("live")) }()
				buf := make([]byte, 4)
				_, err = io.ReadFull(unrelated, buf)
				require.NoError(t, err)
				require.Equal(t, "live", string(buf))
				close(release)
				<-observed.closed
				require.False(t, f.isClosed(), "late-child cleanup must not close the root")
			})
		})
	}
}

func TestHY2ReverseDataDeadlinePreservesOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		late, latePeer := net.Pipe()
		defer latePeer.Close()
		observed := &closeObservedConn{Conn: late, closed: make(chan struct{})}
		f := &gatedDataHY2{closed: make(chan struct{})}
		f.tcp = func(addr string) (net.Conn, error) {
			require.Equal(t, "_hy2tun_data_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa:0", addr)
			select {
			case <-release:
				return observed, nil
			case <-f.closed:
				return nil, net.ErrClosed
			}
		}
		cl, controlPeer := installGatedHY2(t, f)
		local, localPeer := net.Pipe()
		defer localPeer.Close()
		cl.dialLocal = func(context.Context, string, string) (net.Conn, error) { return local, nil }
		cl.mappingConfigs["tcp"] = config.Mapping{ID: "tcp", Type: "tcp", LocalAddr: "127.0.0.1:9", Enabled: true}
		done := make(chan struct{})
		go func() {
			cl.handleOpen(cl.controlStream(), &control.Open{MappingID: "tcp", Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
			close(done)
		}()
		msg, err := control.ReadMessage(controlPeer)
		require.NoError(t, err)
		require.Equal(t, "stream_failed", msg.(*control.OpenResult).Error)
		<-done
		require.False(t, f.isClosed(), "reverse open deadline must not close the root")
		_, err = localPeer.Read(make([]byte, 1))
		require.Error(t, err, "failed reverse open must close its local target")
		close(release)
		<-observed.closed
		require.False(t, f.isClosed())
	})
}

func TestHY2UDPDeadlinePreservesOwner(t *testing.T) {
	for _, phase := range []string{"native", "registration", "registration_owner_close"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				udp := newFakeUDPConn()
				f := &gatedDataHY2{closed: make(chan struct{})}
				f.udp = func() (hyclient.HyUDPConn, error) {
					if phase != "native" {
						return udp, nil
					}
					select {
					case <-release:
						return udp, nil
					case <-f.closed:
						return nil, net.ErrClosed
					}
				}
				if phase != "native" {
					udp.sendFn = func() {
						select {
						case <-release:
						case <-udp.closed:
						}
					}
				}
				cl, _ := installGatedHY2(t, f)
				u, err := cl.startUDP(config.Mapping{ID: "udp", Type: "udp", LocalAddr: "127.0.0.1:9", Enabled: true}, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
				require.Nil(t, u)
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.False(t, f.isClosed(), "UDP open deadline must not close the root")
				if phase == "registration_owner_close" {
					require.NoError(t, cl.Close())
					synctest.Wait()
					require.True(t, udp.isClosed(), "owner close must release pending registration")
					return
				}
				close(release)
				<-udp.closed
				require.False(t, f.isClosed(), "late UDP registration cleanup must not close the root")
			})
		})
	}
}

type cancelAtAdmissionContext struct {
	context.Context
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (c *cancelAtAdmissionContext) Err() error {
	// initial guard, worker result check, then caller admission check.
	if c.calls.Add(1) == 3 {
		c.cancel()
	}
	return c.Context.Err()
}

type blockedChildClose struct {
	net.Conn
	entered, release chan struct{}
	once             sync.Once
}

func (c *blockedChildClose) Close() error {
	c.once.Do(func() { close(c.entered); <-c.release; _ = c.Conn.Close() })
	return nil
}

func TestHY2HandoffCancellationRetainsCleanupBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		late, latePeer := net.Pipe()
		defer latePeer.Close()
		child := &blockedChildClose{Conn: late, entered: make(chan struct{}), release: make(chan struct{})}
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(child.release) })
		live, livePeer := net.Pipe()
		defer livePeer.Close()
		f := &gatedDataHY2{closed: make(chan struct{})}
		f.tcp = func(addr string) (net.Conn, error) {
			if addr == "after.example:443" {
				return live, nil
			}
			return child, nil
		}
		cl, _ := installGatedHY2(t, f)
		// A small reservation pool makes cleanup admission observable; the
		// separate 1024-worker test checks the actual production limit.
		cl.hy2DataOpenSlots = make(chan struct{}, 1)
		base, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx := &cancelAtAdmissionContext{Context: base, cancel: cancel}
		session, _, ownerCtx := cl.sessionFor(nil)
		done := make(chan error, 1)
		go func() {
			_, err := cl.openTCPData(ctx, ownerCtx, session, transport.TCPRequest{Target: "pending.example:443"})
			done <- err
		}()
		require.ErrorIs(t, <-done, context.Canceled, "caller must not wait for blocking late-child cleanup")
		<-child.entered
		_, err := cl.TCP("overflow.example:443")
		require.ErrorIs(t, err, transport.ErrQueueFull, "late-child Close must retain its reservation")
		require.False(t, f.isClosed())
		releaseOnce.Do(func() { close(child.release) })
		synctest.Wait()
		conn, err := cl.TCP("after.example:443")
		require.NoError(t, err, "completed cleanup must release capacity")
		require.NoError(t, conn.Close())
	})
}

func TestHY2CancelledDataOpensRetainBoundedBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{}, 1024)
		var active atomic.Int32
		f := &gatedDataHY2{closed: make(chan struct{})}
		f.tcp = func(string) (net.Conn, error) {
			active.Add(1)
			defer active.Add(-1)
			started <- struct{}{}
			<-f.closed
			return nil, net.ErrClosed
		}
		cl, _ := installGatedHY2(t, f)
		for i := 0; i < 1024; i++ {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { _, err := cl.TCPContext(ctx, "pending.example:443"); done <- err }()
			<-started
			cancel()
			require.ErrorIs(t, <-done, context.Canceled)
			require.False(t, f.isClosed(), "operation cancellation must not reclaim capacity by killing root")
		}
		_, err := cl.TCPContext(context.Background(), "overflow.example:443")
		require.ErrorIs(t, err, transport.ErrQueueFull)
		require.Equal(t, int32(1024), active.Load(), "cancelled pending workers retain their reservations")
		require.NoError(t, cl.Close())
		synctest.Wait()
		require.Zero(t, active.Load(), "owner close must release every pending native operation")
	})
}

func TestAcceptedHelloAckDetachesSetupBeforePersistence(t *testing.T) {
	for _, persistErr := range []error{nil, errors.New("disk unavailable")} {
		name := "success"
		if persistErr != nil {
			name = "write_failure"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := validTestClientConfig()
				cfg.Mappings = []config.Mapping{{ID: "tcp", Type: "tcp", LocalAddr: "127.0.0.1:9", Enabled: true}}
				cl, err := New(cfg, nil)
				require.NoError(t, err)
				defer cl.Close()
				s := newSessionStub(transport.TCP)
				conn, peer := net.Pipe()
				s.control = conn
				defer peer.Close()
				entered, release := make(chan struct{}), make(chan struct{})
				cl.SetPersist(func(*config.ClientConfig) error { close(entered); <-release; return persistErr })
				setup, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				defer cancel()
				lifetime, cancelLifetime := context.WithCancel(context.Background())
				defer cancelLifetime()
				done := make(chan error, 1)
				go func() { _, err := cl.runSessionWithSetup(lifetime, setup, s, 1); done <- err }()
				_, err = control.ReadMessage(peer)
				require.NoError(t, err)
				require.NoError(t, control.WriteMessage(peer, &control.HelloAck{Type: control.TypeHelloAck, OK: true, Results: []control.Result{{ID: "tcp", OK: true, AssignedPort: 37001}}}))
				<-entered
				<-setup.Done()
				synctest.Wait()
				select {
				case <-s.closed:
					t.Error("setup expiry closed an accepted root during persistence")
				default:
				}
				cl.mu.Lock()
				connected := cl.connected
				cl.mu.Unlock()
				require.True(t, connected)
				close(release)
				if persistErr == nil {
					msg, err := control.ReadMessage(peer)
					require.NoError(t, err, "follow-up Update must use lifetime, not expired setup deadline")
					require.IsType(t, &control.Update{}, msg)
				}
				ready, stopReady := context.WithTimeout(context.Background(), time.Second)
				defer stopReady()
				require.NoError(t, cl.WaitReady(ready), "on-time wire Ack must not fail retroactively")
				current := cl.snapshotConfig()
				if persistErr == nil {
					require.Equal(t, 37001, current.Mappings[0].RemotePort)
					require.Equal(t, int64(1), current.Client.Revision)
				} else {
					require.Zero(t, current.Mappings[0].RemotePort)
					require.Zero(t, current.Client.Revision)
				}
				require.NoError(t, control.WriteMessage(peer, &control.Ping{Type: control.TypePing, TS: 123}))
				msg, err := control.ReadMessage(peer)
				require.NoError(t, err)
				require.Equal(t, int64(123), msg.(*control.Pong).TS)
				cancelLifetime()
				<-done
				select {
				case <-s.closed:
				default:
					t.Fatal("lifetime cancellation must still close root")
				}
			})
		})
	}
}

func TestAcceptedHelloAckLifetimeCancelDuringPersistence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := validTestClientConfig()
		cfg.Mappings = []config.Mapping{{ID: "tcp", Type: "tcp", LocalAddr: "127.0.0.1:9", Enabled: true}}
		cl, err := New(cfg, nil)
		require.NoError(t, err)
		defer cl.Close()
		s := newSessionStub(transport.TCP)
		conn, peer := net.Pipe()
		s.control = conn
		defer peer.Close()
		entered, release := make(chan struct{}), make(chan struct{})
		cl.SetPersist(func(*config.ClientConfig) error { close(entered); <-release; return nil })
		setup, cancelSetup := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancelSetup()
		lifetime, cancelLifetime := context.WithCancel(context.Background())
		defer cancelLifetime()
		done := make(chan error, 1)
		go func() { _, err := cl.runSessionWithSetup(lifetime, setup, s, 1); done <- err }()
		_, err = control.ReadMessage(peer)
		require.NoError(t, err)
		require.NoError(t, control.WriteMessage(peer, &control.HelloAck{Type: control.TypeHelloAck, OK: true, Results: []control.Result{{ID: "tcp", OK: true, AssignedPort: 37001}}}))
		<-entered
		cancelLifetime()
		synctest.Wait()
		select {
		case <-s.closed:
		default:
			t.Error("lifetime cancellation failed to close root while persistence was blocked")
		}
		close(release)
		require.Error(t, <-done)
	})
}
