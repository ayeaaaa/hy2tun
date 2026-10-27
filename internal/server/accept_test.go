package server

import (
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// tempAcceptError reports a transient Accept failure such as EMFILE.
type tempAcceptError struct{}

func (tempAcceptError) Error() string   { return "temporary accept failure" }
func (tempAcceptError) Timeout() bool   { return false }
func (tempAcceptError) Temporary() bool { return true }

// flakyListener fails the first Accept and then blocks until closed.
type flakyListener struct {
	mu     sync.Mutex
	calls  int
	closed chan struct{}
	once   sync.Once
}

func newFlakyListener() *flakyListener {
	return &flakyListener{closed: make(chan struct{})}
}

func (l *flakyListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	l.calls++
	call := l.calls
	l.mu.Unlock()
	if call == 1 {
		return nil, tempAcceptError{}
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *flakyListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *flakyListener) Addr() net.Addr { return &net.TCPAddr{} }

// TestServeTCPRetriesTransientAcceptErrors verifies that a temporary Accept
// error does not end the accept loop while the listener is still bound.
func TestServeTCPRetriesTransientAcceptErrors(t *testing.T) {
	srv := &Server{log: slog.Default()}
	runtime := &mappingRuntime{id: "tcp1"}
	listener := newFlakyListener()

	done := make(chan struct{})
	go func() {
		srv.serveTCP(nil, runtime, listener)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("serveTCP returned after a transient Accept error")
	case <-time.After(150 * time.Millisecond):
	}

	require.NoError(t, listener.Close())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveTCP did not return after the listener closed")
	}
}
