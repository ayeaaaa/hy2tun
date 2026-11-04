package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTCPStreamAckRejectsBadKind(t *testing.T) {
	s, peer, ctx := tcpPair(t, RateLimits{}, 0, 0)
	raw, err := s.(*tcpSession).mux.OpenStream()
	require.NoError(t, err)
	defer raw.Close()
	require.NoError(t, writeJSONFrame(raw, StreamHeader{Kind: 99, Token: "sensitive", Target: "private:80"}, 4096))
	_, c, err := peer.Accept(ctx)
	require.Nil(t, c)
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

func TestTCPStreamCancelClosesPending(t *testing.T) {
	s, peer, ctx := tcpPair(t, RateLimits{}, 0, 0)
	opctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		c, e := s.OpenTCP(opctx, TCPRequest{Target: "example.test:80"})
		if c != nil {
			_ = c.Close()
			e = errors.New("delivered before ack")
		}
		done <- e
	}()
	_, c, err := peer.Accept(ctx)
	require.NoError(t, err)
	defer c.Close()
	select {
	case <-done:
		t.Fatal("open did not wait for application ack")
	default:
	}
	cancel()
	require.ErrorIs(t, waitError(t, done), context.Canceled)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = c.Read(make([]byte, 1))
	require.Error(t, err)
	// Unlike HY2's synchronous open, cancelling one pending TCP stream leaves
	// the root session alive.
	done = make(chan error, 1)
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
