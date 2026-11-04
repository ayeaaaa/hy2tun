package transport

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"hy2tun/internal/config"
)

// Close is concurrency-safe but intentionally asymmetric: the first invocation
// closes I/O then waits for cleanup; subsequent Close calls do NOT join it.
// A receipt must track the adapter's callbacks, not infer completion from a
// second native/deferred Close returning on this already-logically-closed child.
type firstProbeClosePending struct {
	net.Conn
	started                    atomic.Bool
	entered, release, finished chan struct{}
}

func (c *firstProbeClosePending) Close() error {
	if c.started.CompareAndSwap(false, true) {
		_ = c.Conn.Close()
		close(c.entered)
		<-c.release
		close(c.finished)
	}
	return nil
}

func TestHY2ProbeCompletionJoinsStartedCloseCallbacks(t *testing.T) {
	for _, shutdown := range []string{"cancel", "root"} {
		t.Run(shutdown, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				local, peer := net.Pipe()
				defer peer.Close()
				child := &firstProbeClosePending{Conn: local, entered: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
				var releaseOnce sync.Once
				defer func() { releaseOnce.Do(func() { close(child.release) }); <-child.finished; synctest.Wait() }()
				f := newFakeHY2()
				f.tcp = func(string) (net.Conn, error) { return child, nil }
				s := WrapHY2(f, "secret").(ProbeCompletionSession)
				defer s.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				type result struct {
					done <-chan struct{}
					err  error
				}
				results := make(chan result, 1)
				go func() { _, done, err := s.ProbeWithCompletion(ctx); results <- result{done, err} }()
				_, err := ReadProbeRequest(peer)
				require.NoError(t, err)
				if shutdown == "cancel" {
					cancel()
				} else {
					require.NoError(t, s.Close())
				}
				res := <-results // Prompt return must not wait for child cleanup.
				if shutdown == "cancel" {
					require.ErrorIs(t, res.err, context.Canceled)
				} else {
					require.ErrorIs(t, res.err, net.ErrClosed)
				}
				<-child.entered
				synctest.Wait()
				select {
				case <-child.finished:
					t.Fatal("cleanup gate did not remain pending")
				default:
				}
				select {
				case <-res.done:
					t.Error("receipt released while an adapter-owned Close callback was still running")
				default:
				}
				releaseOnce.Do(func() { close(child.release) })
				<-child.finished
				<-res.done
			})
		})
	}
}

type probeCloseCount struct {
	net.Conn
	calls atomic.Int32
}

func (c *probeCloseCount) Close() error { c.calls.Add(1); return c.Conn.Close() }

func TestHY2ProbeCompletionStopsCallbacksOnSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		local, peer := net.Pipe()
		defer peer.Close()
		child := &probeCloseCount{Conn: local}
		f := newFakeHY2()
		f.tcp = func(string) (net.Conn, error) { return child, nil }
		s := WrapHY2(f, "secret").(ProbeCompletionSession)
		defer s.Close()
		peerDone := make(chan error, 1)
		go func() {
			req, err := ReadProbeRequest(peer)
			if err == nil {
				err = WriteProbeReply(peer, ProbeReply{OK: true, Info: ProbeInfo{Version: 1, Nonce: req.Nonce, Mode: config.ModeHY2}})
			}
			peerDone <- err
		}()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		info, done, err := s.ProbeWithCompletion(ctx)
		require.NoError(t, err)
		require.Equal(t, config.ModeHY2, info.Mode)
		require.NoError(t, <-peerDone)
		<-done
		select {
		case <-f.closed:
			t.Fatal("successful Probe killed its root")
		default:
		}
		require.Equal(t, int32(1), child.calls.Load())
		cancel()
		require.NoError(t, s.Close())
		synctest.Wait()
		require.Equal(t, int32(1), child.calls.Load(), "completed receipt must leave no callback able to close the child later")
	})
}
