package transport

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"
)

// The initial RED used the same local method set before the public optional
// contract existed. Now verify the exported contract itself.
type testCompletionSession = CompletionSession

type completionBlockedConn struct {
	net.Conn
	entered, release chan struct{}
	once             sync.Once
}

func (c *completionBlockedConn) Close() error {
	c.once.Do(func() { close(c.entered); <-c.release; _ = c.Conn.Close() })
	return nil
}

type completionBlockedUDP struct {
	*fakeDatagram
	entered, release chan struct{}
	once             sync.Once
}

func (d *completionBlockedUDP) Close() error {
	d.once.Do(func() { close(d.entered); <-d.release; _ = d.fakeDatagram.Close() })
	return nil
}

func TestHY2CompletionRetainsNativeAndLateCleanup(t *testing.T) {
	for _, kind := range []string{"tcp", "udp"} {
		for _, shutdown := range []string{"cancel", "close"} {
			t.Run(kind+"/"+shutdown, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					fake := newFakeHY2()
					s := WrapHY2(fake, "secret")
					defer s.Close()
					tracked, ok := s.(testCompletionSession)
					require.True(t, ok, "HY2 must expose actual native/cleanup completion separately from prompt cancellation")
					started, nativeRelease := make(chan struct{}), make(chan struct{})
					entered, cleanupRelease := make(chan struct{}), make(chan struct{})
					var nativeOnce, cleanupOnce sync.Once
					defer nativeOnce.Do(func() { close(nativeRelease) })
					defer cleanupOnce.Do(func() { close(cleanupRelease) })
					local, peer := net.Pipe()
					defer local.Close()
					defer peer.Close()
					child := &completionBlockedConn{Conn: local, entered: entered, release: cleanupRelease}
					udp := &completionBlockedUDP{fakeDatagram: newFakeDatagram(), entered: entered, release: cleanupRelease}
					fake.tcp = func(string) (net.Conn, error) { close(started); <-nativeRelease; return child, nil }
					fake.udp = func() (hyclient.HyUDPConn, error) { close(started); <-nativeRelease; return udp, nil }
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					type result struct {
						completion <-chan struct{}
						err        error
					}
					results := make(chan result, 1)
					go func() {
						var completion <-chan struct{}
						var err error
						if kind == "tcp" {
							conn, done, e := tracked.OpenTCPWithCompletion(ctx, TCPRequest{Target: "example.test:443"})
							if conn != nil {
								_ = conn.Close()
							}
							completion, err = done, e
						} else {
							conn, done, e := tracked.OpenUDPWithCompletion(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
							if conn != nil {
								_ = conn.Close()
							}
							completion, err = done, e
						}
						results <- result{completion, err}
					}()
					<-started
					want := error(context.Canceled)
					if shutdown == "cancel" {
						cancel()
					} else {
						require.NoError(t, s.Close())
						want = net.ErrClosed
					}
					res := <-results
					require.ErrorIs(t, res.err, want)
					require.NotNil(t, res.completion)
					select {
					case <-res.completion:
						t.Error("prompt cancellation was mistaken for native completion")
					default:
					}
					nativeOnce.Do(func() { close(nativeRelease) })
					<-entered
					select {
					case <-res.completion:
						t.Error("native return was mistaken for abandoned-child cleanup completion")
					default:
					}
					cleanupOnce.Do(func() { close(cleanupRelease) })
					<-res.completion
				})
			})
		}
	}
}

type completionHandoffContext struct {
	context.Context
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (c *completionHandoffContext) Err() error {
	if c.calls.Add(1) == 3 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestHY2CompletionCoversCancelledHandoffCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		local, peer := net.Pipe()
		defer local.Close()
		defer peer.Close()
		child := &completionBlockedConn{Conn: local, entered: make(chan struct{}), release: make(chan struct{})}
		defer close(child.release)
		fake := newFakeHY2()
		fake.tcp = func(string) (net.Conn, error) { return child, nil }
		s := WrapHY2(fake, "secret").(CompletionSession)
		defer s.Close()
		base, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx := &completionHandoffContext{Context: base, cancel: cancel}
		conn, completion, err := s.OpenTCPWithCompletion(ctx, TCPRequest{Target: "example.test:443"})
		require.True(t, conn == nil)
		require.ErrorIs(t, err, context.Canceled, "cancelled handoff must return before blocked cleanup finishes")
		<-child.entered
		select {
		case <-completion:
			t.Fatal("cancelled handoff receipt closed before abandoned child Close finished")
		default:
		}
	})
}

func TestHY2CompletionDoesNotWaitForDeliveredChild(t *testing.T) {
	for _, kind := range []string{"tcp", "udp"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				local, peer := net.Pipe()
				defer local.Close()
				defer peer.Close()
				udp := newFakeDatagram()
				fake := newFakeHY2()
				fake.tcp = func(string) (net.Conn, error) { return local, nil }
				fake.udp = func() (hyclient.HyUDPConn, error) { return udp, nil }
				s := WrapHY2(fake, "secret").(CompletionSession)
				defer s.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var completion <-chan struct{}
				if kind == "tcp" {
					conn, done, err := s.OpenTCPWithCompletion(ctx, TCPRequest{Target: "example.test:443"})
					require.NoError(t, err)
					defer conn.Close()
					completion = done
				} else {
					conn, done, err := s.OpenUDPWithCompletion(ctx, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
					require.NoError(t, err)
					defer conn.Close()
					completion = done
				}
				select {
				case <-completion:
				case <-time.After(time.Second):
					t.Fatal("receipt incorrectly waited for delivered child's lifetime")
				}
				cancel()
				select {
				case <-fake.closed:
					t.Fatal("post-handoff context cancellation closed root")
				default:
				}
				if kind == "tcp" {
					go func() { _, _ = local.Write([]byte("live")) }()
					buf := make([]byte, 4)
					n, err := peer.Read(buf)
					require.NoError(t, err)
					require.Equal(t, "live", string(buf[:n]))
				} else {
					require.NoError(t, udp.Send([]byte("live"), "127.0.0.1:1234"))
					<-udp.sent // registration
					msg := <-udp.sent
					require.Equal(t, []byte("live"), msg.data)
				}
			})
		})
	}
}

func TestHY2CompletionNoWorkerReturnsClosedReceipt(t *testing.T) {
	fake := newFakeHY2()
	s := WrapHY2(fake, "secret").(CompletionSession)
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, completion, err := s.OpenTCPWithCompletion(ctx, TCPRequest{Target: "example.test:443"})
	require.True(t, conn == nil)
	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-completion:
	default:
		t.Fatal("pre-cancelled open did not return a completed receipt")
	}
	conn, completion, err = s.OpenTCPWithCompletion(context.Background(), TCPRequest{})
	require.True(t, conn == nil)
	require.ErrorIs(t, err, ErrProtocol)
	select {
	case <-completion:
	default:
		t.Fatal("invalid TCP request did not return a completed receipt")
	}
	udp, completion, err := s.OpenUDPWithCompletion(context.Background(), "invalid")
	require.True(t, udp == nil)
	require.ErrorIs(t, err, ErrProtocol)
	select {
	case <-completion:
	default:
		t.Fatal("invalid UDP request did not return a completed receipt")
	}
	select {
	case <-fake.closed:
		t.Fatal("rejected request closed the existing root")
	default:
	}
}
