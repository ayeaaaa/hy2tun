package transport

import (
	"context"
	"net"
	"sync"
	"testing"
	"testing/synctest"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"
)

type setupBlockedRoot struct {
	*fakeHY2
	entered, release chan struct{}
	once             sync.Once
}

func (f *setupBlockedRoot) Close() error {
	f.once.Do(func() { close(f.entered); <-f.release; _ = f.fakeHY2.Close() })
	return nil
}

func TestHY2DialCompletionRetainsFactoryAndLateRootCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		nativeRelease := make(chan struct{})
		f := &setupBlockedRoot{fakeHY2: newFakeHY2(), entered: make(chan struct{}), release: make(chan struct{})}
		var nativeOnce, cleanupOnce sync.Once
		defer nativeOnce.Do(func() { close(nativeRelease) })
		defer cleanupOnce.Do(func() { close(f.release) })
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type result struct {
			done <-chan struct{}
			err  error
		}
		results := make(chan result, 1)
		go func() {
			_, done, err := DialHY2WithCompletion(ctx, DialOptions{HY2: &hyclient.Config{}}, func(*hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error) {
				<-nativeRelease
				return f, nil, nil
			})
			results <- result{done, err}
		}()
		synctest.Wait()
		cancel()
		res := <-results
		require.ErrorIs(t, res.err, context.Canceled)
		require.NotNil(t, res.done)
		select {
		case <-res.done:
			t.Fatal("cancellation completed the factory receipt")
		default:
		}
		nativeOnce.Do(func() { close(nativeRelease) })
		<-f.entered
		select {
		case <-res.done:
			t.Fatal("factory return completed the receipt before root Close")
		default:
		}
		cleanupOnce.Do(func() { close(f.release) })
		<-res.done
	})
}

func TestHY2ProbeCompletionRetainsNativeAndChildCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFakeHY2()
		s := WrapHY2(f, "secret").(ProbeCompletionSession)
		defer s.Close()
		nativeRelease := make(chan struct{})
		local, peer := net.Pipe()
		defer peer.Close()
		child := &completionBlockedConn{Conn: local, entered: make(chan struct{}), release: make(chan struct{})}
		var nativeOnce, cleanupOnce sync.Once
		defer nativeOnce.Do(func() { close(nativeRelease) })
		defer cleanupOnce.Do(func() { close(child.release) })
		f.tcp = func(string) (net.Conn, error) { <-nativeRelease; return child, nil }
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type result struct {
			done <-chan struct{}
			err  error
		}
		results := make(chan result, 1)
		go func() { _, done, err := s.ProbeWithCompletion(ctx); results <- result{done, err} }()
		synctest.Wait()
		cancel()
		res := <-results
		require.ErrorIs(t, res.err, context.Canceled)
		require.NotNil(t, res.done)
		select {
		case <-res.done:
			t.Fatal("root cancellation completed the probe receipt")
		default:
		}
		nativeOnce.Do(func() { close(nativeRelease) })
		<-child.entered
		select {
		case <-res.done:
			t.Fatal("native return completed the receipt before child Close")
		default:
		}
		cleanupOnce.Do(func() { close(child.release) })
		<-res.done
	})
}

func TestHY2SetupCompletionNoWorkerAndSuccessLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, done, err := DialHY2WithCompletion(ctx, DialOptions{}, nil)
		require.ErrorIs(t, err, context.Canceled)
		select {
		case <-done:
		default:
			t.Fatal("pre-cancelled dial receipt pending")
		}
		f := newFakeHY2()
		s := WrapHY2(f, "secret")
		defer s.Close()
		_, done, err = s.(ProbeCompletionSession).ProbeWithCompletion(ctx)
		require.ErrorIs(t, err, context.Canceled)
		select {
		case <-done:
		default:
			t.Fatal("pre-cancelled probe receipt pending")
		}
		_, done, err = s.(ControlCompletionSession).OpenControlWithCompletion(ctx)
		require.ErrorIs(t, err, context.Canceled)
		select {
		case <-done:
		default:
			t.Fatal("pre-cancelled control receipt pending")
		}
		live, cancelLive := context.WithCancel(context.Background())
		defer cancelLive()
		root, done, err := DialHY2WithCompletion(live, DialOptions{HY2: &hyclient.Config{}}, func(*hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error) { return f, nil, nil })
		require.NoError(t, err)
		defer root.Close()
		<-done
		cancelLive()
		select {
		case <-f.closed:
			t.Fatal("completed dial receipt retained delivered root lifetime")
		default:
		}
	})
}
