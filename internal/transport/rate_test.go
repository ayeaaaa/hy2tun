package transport

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Both server directions and both client directions are independent enforcement
// points. The unthrottled opposite side may complete into the bounded mux window.
func TestTCPRateLimitCancels(t *testing.T) {
	for _, side := range []string{"client_up", "client_down", "server_up", "server_down"} {
		t.Run(side, func(t *testing.T) {
			var limits RateLimits
			var up, down uint64
			switch side {
			case "client_up":
				up = 1
			case "client_down":
				down = 1
			case "server_up":
				limits.Up = 1
			case "server_down":
				limits.Down = 1
			}
			s, peer, ctx := tcpPair(t, limits, up, down)
			done := make(chan error, 1)
			go func() {
				_, c, e := peer.Accept(ctx)
				if e == nil {
					e = WriteStreamAck(c, nil)
				}
				if e == nil {
					if side == "client_up" || side == "server_up" {
						_, e = io.Copy(io.Discard, c)
					} else {
						_, e = c.Write(make([]byte, 32768))
					}
				}
				done <- e
			}()
			c, err := s.OpenTCP(ctx, TCPRequest{Target: "example.test:80"})
			require.NoError(t, err)
			operation := make(chan error, 1)
			go func() {
				var e error
				if side == "client_up" || side == "server_up" {
					_, e = c.Write(make([]byte, 32768))
				} else {
					_, e = io.ReadFull(c, make([]byte, 32768))
				}
				operation <- e
			}()
			select {
			case e := <-operation:
				if side != "server_up" {
					t.Fatalf("rate limit bypassed: %v", e)
				}
			case <-time.After(40 * time.Millisecond):
			}
			require.NoError(t, s.Close())
			require.NoError(t, peer.Close())
			if side != "server_up" {
				require.Error(t, waitError(t, operation))
			}
			peerErr := waitError(t, done)
			if side != "client_down" {
				require.Error(t, peerErr)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, newByteLimiter(1).wait(ctx, 16384), context.Canceled)
}

func TestTCPRateSharedAcrossStreamsAndDeadlineChanges(t *testing.T) {
	s, peer, ctx := tcpPair(t, RateLimits{}, 1, 0)
	done := make(chan error, 1)
	accept := func() {
		_, c, e := peer.Accept(ctx)
		if e == nil {
			defer c.Close()
			e = WriteStreamAck(c, nil)
		}
		if e == nil {
			_, e = io.Copy(io.Discard, c)
		}
		done <- e
	}
	go accept()
	first, err := s.OpenTCP(ctx, TCPRequest{Target: "example.test:80"})
	require.NoError(t, err)
	_, err = first.Write(make([]byte, 16384))
	require.NoError(t, err)
	go accept()
	second, err := s.OpenTCP(ctx, TCPRequest{Target: "example.test:81"})
	require.NoError(t, err)
	op := make(chan error, 1)
	go func() { _, e := second.Write([]byte{1}); op <- e }()
	select {
	case e := <-op:
		t.Fatalf("second stream bypassed shared limiter: %v", e)
	case <-time.After(30 * time.Millisecond):
	}
	require.NoError(t, second.SetWriteDeadline(time.Now().Add(20*time.Millisecond)))
	err = waitError(t, op)
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	var timeout net.Error
	require.ErrorAs(t, err, &timeout)
	require.True(t, timeout.Timeout())
	require.NoError(t, first.Close())
	require.NoError(t, second.Close())
	for i := 0; i < 2; i++ {
		require.NoError(t, waitError(t, done))
	}
}
