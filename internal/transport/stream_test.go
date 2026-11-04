package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/fatedier/yamux"
	"github.com/stretchr/testify/require"
)

func TestTCPStreamAckRejectsBadKind(t *testing.T) {
	s, peer, ctx := tcpPair(t, RateLimits{}, 0, 0)
	raw, err := s.(*tcpSession).mux.OpenStream()
	require.NoError(t, err)
	defer raw.Close()
	require.NoError(t, writeJSONFrame(raw, StreamHeader{Kind: 99, Token: "sensitive", Target: "private:80"}, 4096))
	_, c, err := peer.Accept(ctx)
	require.True(t, c == nil, "failed Accept must return a nil net.Conn interface, not a typed-nil pointer")
	require.ErrorIs(t, err, ErrProtocol)
	var ack streamAck
	require.NoError(t, readJSONFrame(raw, &ack, 4096))
	require.False(t, ack.OK)
	require.Equal(t, "bad_message", ack.Error)
	// Rejection is scoped to the stream; control remains usable and ack-gated.
	done := make(chan error, 1)
	go func() {
		h, c, e := peer.Accept(ctx)
		if e == nil && h.Kind != StreamControl {
			e = errors.New("wrong control header")
		}
		if e == nil {
			e = WriteStreamAck(c, nil)
		}
		if e == nil {
			_, e = c.Write([]byte("ok"))
		}
		done <- e
	}()
	control, err := s.OpenControl(ctx)
	require.NoError(t, err)
	defer control.Close()
	b := make([]byte, 2)
	_, err = io.ReadFull(control, b)
	require.NoError(t, err)
	require.Equal(t, "ok", string(b))
	require.NoError(t, waitError(t, done))
}

func TestTCPStreamRejectedOpenReturnsNilInterface(t *testing.T) {
	for _, reject := range []error{ErrAuthentication, ErrProtocol} {
		t.Run(reject.Error(), func(t *testing.T) {
			s, peer, ctx := tcpPair(t, RateLimits{}, 0, 0)
			done := make(chan error, 1)
			go func() {
				_, c, e := peer.Accept(ctx)
				if e == nil {
					defer c.Close()
					e = WriteStreamAck(c, reject)
				}
				done <- e
			}()
			c, err := s.OpenControl(ctx)
			require.ErrorIs(t, err, reject)
			require.True(t, c == nil, "a rejected application ack must return a nil net.Conn interface")
			// Exercise the ordinary cleanup guard which would panic on typed nil.
			require.NotPanics(t, func() {
				if c != nil {
					_ = c.Close()
				}
			})
			require.NoError(t, waitError(t, done))
		})
	}
}

// A failed limiter wait must not skip consumed bytes, and retrying pending
// bytes must still acquire tokens rather than bypass the configured limit.
func TestTCPStreamReadDeadlineRetainsBytesAndRate(t *testing.T) {
	s, peer, ctx := tcpPair(t, RateLimits{}, 0, 1024)
	done := make(chan error, 1)
	go func() {
		_, c, e := peer.Accept(ctx)
		if e == nil {
			defer c.Close()
			e = WriteStreamAck(c, nil)
		}
		if e == nil {
			payload := append(make([]byte, 16384), bytes.Repeat([]byte("L"), 64)...)
			payload = append(payload, []byte("TAIL")...)
			_, e = c.Write(payload)
		}
		done <- e
	}()
	c, err := s.OpenTCP(ctx, TCPRequest{Target: "example.test:80"})
	require.NoError(t, err)
	defer c.Close()
	_, err = io.ReadFull(c, make([]byte, 16384))
	require.NoError(t, err)
	require.NoError(t, waitError(t, done))
	for i := 0; i < 2; i++ {
		require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Millisecond)))
		n, e := c.Read(make([]byte, 64))
		require.Zero(t, n, "deadline retries must not deliver unthrottled pending bytes")
		require.ErrorIs(t, e, os.ErrDeadlineExceeded)
	}
	require.NoError(t, c.SetReadDeadline(time.Time{}))
	first := make([]byte, 4)
	_, err = io.ReadFull(c, first)
	require.NoError(t, err)
	require.Equal(t, "LLLL", string(first), "retry must return the unreported prefix, not TAIL")
	rest := make([]byte, 64)
	_, err = io.ReadFull(c, rest)
	require.NoError(t, err)
	require.Equal(t, string(bytes.Repeat([]byte("L"), 60))+"TAIL", string(rest))
	require.NoError(t, c.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = c.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
}

func TestTCPStreamCancelClosesPending(t *testing.T) {
	s, peer, ctx := tcpPair(t, RateLimits{}, 0, 0)
	opctx, cancel := context.WithCancel(ctx)
	outcomes := make(chan streamOutcome, 1)
	go func() {
		c, e := s.OpenTCP(opctx, TCPRequest{Target: "example.test:80"})
		outcomes <- streamOutcome{conn: c, err: e}
	}()
	_, c, err := peer.Accept(ctx)
	require.NoError(t, err)
	defer c.Close()
	select {
	case <-outcomes:
		t.Fatal("open did not wait for application ack")
	default:
	}
	cancel()
	r := waitStreamOutcome(t, outcomes)
	require.ErrorIs(t, r.err, context.Canceled)
	require.True(t, r.conn == nil, "cancelled OpenTCP must return a nil net.Conn interface")
	require.NoError(t, c.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = c.Read(make([]byte, 1))
	require.Error(t, err)
	// Unlike HY2's synchronous open, cancelling one pending TCP stream leaves
	// the root session alive.
	done := make(chan error, 1)
	go func() {
		_, c, e := peer.Accept(ctx)
		if e == nil {
			e = WriteStreamAck(c, nil)
		}
		done <- e
	}()
	control, err := s.OpenControl(ctx)
	require.NoError(t, err)
	defer control.Close()
	require.NoError(t, waitError(t, done))
}

func TestTCPStreamAckSanitizesError(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	done := make(chan error, 1)
	go func() { done <- WriteStreamAck(local, errors.New("token=private target=secret")) }()
	var ack streamAck
	require.NoError(t, readJSONFrame(remote, &ack, 4096))
	require.False(t, ack.OK)
	require.Equal(t, "bad_message", ack.Error)
	require.NoError(t, waitError(t, done))
}

func TestTCPActiveStreamLimitAndRelease(t *testing.T) {
	s, peer, ctx := tcpPair(t, RateLimits{}, 0, 0)
	accepted := make(chan net.Conn, 1)
	done := make(chan error, 1)
	released := make(chan struct{})
	go func() {
		for i := 0; i < 1025; i++ {
			if i == 1024 {
				select {
				case <-released:
				case <-ctx.Done():
					done <- ctx.Err()
					return
				}
			}
			_, c, e := peer.Accept(ctx)
			if e != nil {
				done <- e
				return
			}
			if e = WriteStreamAck(c, nil); e != nil {
				done <- e
				return
			}
			accepted <- c
		}
		done <- nil
	}()
	var local, remote []net.Conn
	defer func() {
		for _, c := range local {
			_ = c.Close()
		}
		for _, c := range remote {
			_ = c.Close()
		}
	}()
	for i := 0; i < 1024; i++ {
		c, e := s.OpenControl(ctx)
		require.NoError(t, e)
		local = append(local, c)
		remote = append(remote, <-accepted)
	}
	_, err := s.OpenControl(ctx)
	require.ErrorIs(t, err, ErrQueueFull)
	require.NoError(t, local[0].Close())
	require.NoError(t, remote[0].Close())
	require.Eventually(t, func() bool { return s.(*tcpSession).mux.NumStreams() < 1024 }, time.Second, time.Millisecond)
	close(released)
	c, err := s.OpenControl(ctx)
	require.NoError(t, err)
	defer c.Close()
	r := <-accepted
	defer r.Close()
	require.NoError(t, waitError(t, done))
}

// SYNs are deliberately queued before Accept starts. The queued mux stream
// already exists; accepting it consumes ownership capacity, not another stream.
func TestTCPStreamAcceptQueuedAtCapacity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		owned, queued int
	}{
		{"last_queued", 1023, 1},
		{"full_backlog", 768, 256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, peer, ctx := tcpPair(t, RateLimits{}, 0, 0)
			held := holdTCPControls(t, s, peer, ctx, tc.owned)
			var queued []*yamux.Stream
			var delivered []net.Conn
			defer func() {
				for _, c := range delivered {
					_ = c.Close()
				}
				for _, c := range queued {
					_ = c.Close()
				}
			}()
			for i := 0; i < tc.queued; i++ {
				c, e := s.(*tcpSession).mux.OpenStream()
				require.NoError(t, e)
				queued = append(queued, c)
				require.NoError(t, writeJSONFrame(c, StreamHeader{Kind: StreamControl}, 4096))
			}
			require.Eventually(t, func() bool { return peer.session.mux.NumStreams() == 1024 }, time.Second, time.Millisecond)
			t.Logf("before Accept: owned slots=%d, mux streams=%d", len(peer.session.slots), peer.session.mux.NumStreams())
			_, err := s.OpenControl(ctx)
			require.ErrorIs(t, err, ErrQueueFull, "outbound creation must still honor the mux stream budget")
			for _, raw := range queued {
				h, c, e := peer.Accept(ctx)
				require.NoError(t, e, "already queued streams must use the free ownership slots")
				require.Equal(t, StreamControl, h.Kind)
				delivered = append(delivered, c)
				require.NoError(t, WriteStreamAck(c, nil))
				var ack streamAck
				require.NoError(t, readJSONFrame(raw, &ack, 4096))
				require.True(t, ack.OK)
			}
			// A further pending SYN must not create a 1025th owned connection.
			extra, e := s.(*tcpSession).mux.OpenStream()
			require.NoError(t, e)
			defer extra.Close()
			require.NoError(t, writeJSONFrame(extra, StreamHeader{Kind: StreamControl}, 4096))
			require.Eventually(t, func() bool { return peer.session.mux.NumStreams() == 1025 }, time.Second, time.Millisecond)
			_, c, e := peer.Accept(ctx)
			require.True(t, c == nil, "capacity rejection must return a nil net.Conn interface")
			require.ErrorIs(t, e, ErrQueueFull)
			require.NoError(t, held[0].Close())
			require.NoError(t, held[1].Close())
			_, c, e = peer.Accept(ctx)
			require.NoError(t, e)
			defer c.Close()
			require.NoError(t, WriteStreamAck(c, nil))
			var ack streamAck
			require.NoError(t, readJSONFrame(extra, &ack, 4096))
			require.True(t, ack.OK)
		})
	}
}

func holdTCPControls(t *testing.T, s Session, peer *TCPPeer, ctx context.Context, n int) []net.Conn {
	t.Helper()
	accepted := make(chan net.Conn, 1)
	done := make(chan error, 1)
	go func() {
		for i := 0; i < n; i++ {
			_, c, e := peer.Accept(ctx)
			if e == nil {
				e = WriteStreamAck(c, nil)
			}
			if e != nil {
				done <- e
				return
			}
			select {
			case accepted <- c:
			case <-ctx.Done():
				_ = c.Close()
				done <- ctx.Err()
				return
			}
		}
		done <- nil
	}()
	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})
	for i := 0; i < n; i++ {
		c, e := s.OpenControl(ctx)
		require.NoError(t, e)
		held = append(held, c)
		select {
		case c = <-accepted:
			held = append(held, c)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	require.NoError(t, waitError(t, done))
	return held
}

// This is the review's real committed-RST reproduction. Cancellation must
// return to the caller, but neither cleanup nor its open worker may escape the
// budget while yamux is still blocked writing the reset.
func TestTCPStreamCancelledCleanupRetainsWorkerBudget(t *testing.T) {
	s, peer, gate, ctx := gatedTCPMuxPair(t, 8, false)
	pending, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := s.OpenControl(pending); done <- e }()
	_, first, err := peer.Accept(ctx)
	require.NoError(t, err)
	defer first.Close()
	cancel()
	require.ErrorIs(t, waitError(t, done), context.Canceled)
	waitSignal(t, gate.started)
	require.Equal(t, 1, len(s.slots), "a committed reset must retain the cancelled operation's reservation")
	results := make(chan error, 1024)
	var callers sync.WaitGroup
	for i := 0; i < 1024; i++ {
		callers.Add(1)
		go func() { defer callers.Done(); _, e := s.OpenControl(ctx); results <- e }()
	}
	require.Eventually(t, func() bool { return len(s.slots) == 1024 }, time.Second, time.Millisecond)
	require.ErrorIs(t, waitError(t, results), ErrQueueFull, "the retained cleanup slot must reject one of 1024 new opens")
	stack := make([]byte, 4<<20)
	count := bytes.Count(stack[:runtime.Stack(stack, true)], []byte("hy2tun/internal/transport.(*tcpSession).openStream.func1()"))
	t.Logf("owned slots=%d; still-live open workers=%d, including cancelled cleanup if still running", len(s.slots), count)
	require.LessOrEqual(t, count, 1024)
	gate.unblock()
	require.NoError(t, s.Close())
	require.NoError(t, peer.Close())
	finished := make(chan struct{})
	go func() { callers.Wait(); close(finished) }()
	waitSignal(t, finished)
	require.Eventually(t, func() bool { return len(s.slots) == 0 }, time.Second, time.Millisecond)
}

func TestTCPStreamCloseRetainsSlotUntilFINCompletes(t *testing.T) {
	s, peer, gate, ctx := gatedTCPMuxPair(t, 4, false)
	accepted := make(chan net.Conn, 1)
	done := make(chan error, 1)
	go func() {
		_, c, e := peer.Accept(ctx)
		if e == nil {
			e = WriteStreamAck(c, nil)
			accepted <- c
		}
		done <- e
	}()
	c, err := s.OpenControl(ctx)
	require.NoError(t, err)
	require.NoError(t, waitError(t, done))
	remote := <-accepted
	defer remote.Close()
	go func() { done <- c.Close() }()
	waitSignal(t, gate.started)
	require.Equal(t, 1, len(s.slots), "normal Close must retain ownership until its committed FIN finishes")
	gate.unblock()
	require.NoError(t, waitError(t, done))
	require.Eventually(t, func() bool { return len(s.slots) == 0 }, time.Second, time.Millisecond)
}

func TestTCPStreamAcceptCancellationRetainsCleanupBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags uint16
	}{{"header_reset", 8}, {"committed_mux_ack", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			s, peer, gate, ctx := gatedTCPMuxPair(t, tc.flags, true)
			raw, err := s.mux.OpenStream()
			require.NoError(t, err)
			defer raw.Close()
			pending, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan streamOutcome, 1)
			go func() {
				_, c, e := peer.Accept(pending)
				done <- streamOutcome{conn: c, err: e}
			}()
			if tc.flags == 8 {
				waitSignal(t, gate.ackSent)
			} else {
				waitSignal(t, gate.started)
			}
			cancel()
			r := waitStreamOutcome(t, done)
			require.ErrorIs(t, r.err, context.Canceled)
			require.True(t, r.conn == nil, "cancelled Accept must return a nil net.Conn interface")
			if tc.flags == 8 {
				waitSignal(t, gate.started)
			}
			require.Equal(t, 1, len(peer.session.slots), "accept cancellation must not release blocked reset/ack cleanup")
			gate.unblock()
			require.Eventually(t, func() bool { return len(peer.session.slots) == 0 }, time.Second, time.Millisecond)
		})
	}
}

type streamOutcome struct {
	conn net.Conn
	err  error
}

func waitStreamOutcome(t *testing.T, ch <-chan streamOutcome) streamOutcome {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("stream operation did not finish")
		return streamOutcome{}
	}
}

// Only transport output is gated; the real mux owns state, stream registration,
// deadlines, reset/close semantics and all worker operations.
func gatedTCPMuxPair(t *testing.T, flags uint16, serverGate bool) (*tcpSession, *TCPPeer, *streamOutputGate, context.Context) {
	t.Helper()
	local, remote := net.Pipe()
	gate := &streamOutputGate{flags: flags, started: make(chan struct{}), released: make(chan struct{}), closed: make(chan struct{}), ackSent: make(chan struct{})}
	var clientConn, serverConn net.Conn = local, remote
	if serverGate {
		gate.Conn = remote
		serverConn = gate
	} else {
		gate.Conn = local
		clientConn = gate
	}
	clientMux, err := yamux.Client(clientConn, tcpMuxConfig())
	require.NoError(t, err)
	serverMux, err := yamux.Server(serverConn, tcpMuxConfig())
	require.NoError(t, err)
	s := newTCPSession(clientMux, 0, 0)
	peer := &TCPPeer{session: newTCPSession(serverMux, 0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
		_ = peer.Close()
		require.Eventually(t, func() bool { return len(s.slots) == 0 && len(peer.session.slots) == 0 }, time.Second, time.Millisecond)
	})
	return s, peer, gate, ctx
}

type streamOutputGate struct {
	net.Conn
	flags                                      uint16
	deadline                                   deadlineState
	started, released, closed, ackSent         chan struct{}
	startOnce, releaseOnce, closeOnce, ackOnce sync.Once
}

func (c *streamOutputGate) Write(p []byte) (int, error) {
	if len(p) != 12 || p[0] != 0 || p[1] != 1 || binary.BigEndian.Uint16(p[2:4])&c.flags == 0 {
		n, err := c.Conn.Write(p)
		if err == nil && len(p) == 12 && p[0] == 0 && p[1] == 1 && binary.BigEndian.Uint16(p[2:4])&2 != 0 {
			c.ackOnce.Do(func() { close(c.ackSent) })
		}
		return n, err
	}
	c.startOnce.Do(func() { close(c.started) })
	for {
		at, changed := c.deadline.snapshot()
		var timer *time.Timer
		var timeout <-chan time.Time
		if !at.IsZero() {
			timer = time.NewTimer(time.Until(at))
			timeout = timer.C
		}
		select {
		case <-c.closed:
			if timer != nil {
				timer.Stop()
			}
			return 0, net.ErrClosed
		case <-c.released:
			if timer != nil {
				timer.Stop()
			}
			return c.Conn.Write(p)
		case <-timeout:
			return 0, os.ErrDeadlineExceeded
		case <-changed:
			if timer != nil {
				timer.Stop()
			}
		}
	}
}
func (c *streamOutputGate) SetWriteDeadline(t time.Time) error {
	c.deadline.set(t)
	return c.Conn.SetWriteDeadline(t)
}
func (c *streamOutputGate) unblock() { c.releaseOnce.Do(func() { close(c.released) }) }
func (c *streamOutputGate) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
