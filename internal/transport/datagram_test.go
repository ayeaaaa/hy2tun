package transport

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Literal wire fixtures catch including the uint32 prefix in frameLen, merging
// packets, and failing to apply either independent metadata/payload bound.
func TestDatagramFrameBoundaries(t *testing.T) {
	r := &oneByteReader{r: bytes.NewReader([]byte{0, 0, 0, 5, 0, 1, 'a', 'x', 'y', 0, 0, 0, 2, 0, 0})}
	p, addr, err := ReadDatagramFrame(r)
	require.NoError(t, err)
	require.Equal(t, "a", addr)
	require.Equal(t, []byte("xy"), p)
	p, addr, err = ReadDatagramFrame(r)
	require.NoError(t, err)
	require.Empty(t, addr)
	require.Empty(t, p)
	var b bytes.Buffer
	require.NoError(t, WriteDatagramFrame(&b, bytes.Repeat([]byte{7}, 4096), strings.Repeat("a", 256)))
	require.Equal(t, []byte{0, 0, 17, 2, 1, 0}, b.Bytes()[:6])
	p, addr, err = ReadDatagramFrame(&oneByteReader{r: &b})
	require.NoError(t, err)
	require.Len(t, p, 4096)
	require.Len(t, addr, 256)
	for _, tc := range []struct {
		payload int
		addr    int
	}{{4097, 0}, {0, 257}} {
		require.ErrorIs(t, WriteDatagramFrame(&b, make([]byte, tc.payload), strings.Repeat("a", tc.addr)), ErrProtocol)
	}
}

func TestDatagramRejectsOversize(t *testing.T) {
	for _, prefix := range [][]byte{{0x40, 0, 0, 0}, {0, 0, 0, 1}, {0, 0, 17, 3}} {
		r := &prefixOnlyReader{prefix: prefix}
		_, _, err := ReadDatagramFrame(r)
		require.ErrorIs(t, err, ErrProtocol)
		require.False(t, r.bodyRead)
	}
	for _, frame := range [][]byte{{0, 0, 0, 2, 1, 1}, append([]byte{0, 0, 16, 3, 0, 0}, make([]byte, 4097)...)} {
		_, _, err := ReadDatagramFrame(bytes.NewReader(frame))
		require.ErrorIs(t, err, ErrProtocol)
	}
}

func TestTCPUDPQueueBounded(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	started := make(chan struct{})
	conn := NewDatagramConn(&writeStartedConn{Conn: local, started: started})
	defer conn.Close()
	require.NoError(t, conn.Send([]byte("blocked"), "peer"))
	waitSignal(t, started)
	for i := 0; i < 32; i++ {
		require.NoError(t, conn.Send([]byte{byte(i)}, "peer"))
	}
	require.ErrorIs(t, conn.Send([]byte("overflow"), "peer"), ErrQueueFull)
	done := make(chan error, 1)
	go func() { _, _, err := conn.Receive(); done <- err }()
	require.NoError(t, conn.Close())
	require.Error(t, waitError(t, done))
	require.ErrorIs(t, conn.Send(nil, "peer"), net.ErrClosed)
}

func TestTCPUDPBackpressureDoesNotBlockControl(t *testing.T) {
	s, peer, ctx := tcpPair(t, RateLimits{}, 1, 0)
	done := make(chan error, 1)
	go func() {
		_, c, e := peer.Accept(ctx)
		if e == nil {
			defer c.Close()
			e = WriteStreamAck(c, nil)
		}
		if e == nil {
			_, e = io.ReadFull(c, make([]byte, 16384))
		}
		done <- e
	}()
	c, err := s.OpenTCP(ctx, TCPRequest{Target: "example.test:80"})
	require.NoError(t, err)
	_, err = c.Write(make([]byte, 16384))
	require.NoError(t, err)
	require.NoError(t, waitError(t, done))
	require.NoError(t, c.Close())
	accepted := make(chan net.Conn, 1)
	go func() {
		_, c, e := peer.Accept(ctx)
		if e == nil {
			e = WriteStreamAck(c, nil)
			accepted <- c
		}
		done <- e
	}()
	d, err := s.OpenUDP(ctx, "0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	defer d.Close()
	require.NoError(t, waitError(t, done))
	udpPeer := <-accepted
	defer udpPeer.Close()
	require.NoError(t, d.Send([]byte{1}, "p"))
	// Wait for the sender to own its single blocked frame before filling the
	// remaining queue. Channel length is only a scheduling barrier here.
	require.Eventually(t, func() bool { return len(d.(*tcpDatagram).queue) == 0 }, time.Second, time.Millisecond)
	for i := 0; i < 32; i++ {
		require.NoError(t, d.Send([]byte{byte(i)}, "p"))
	}
	require.ErrorIs(t, d.Send(nil, "p"), ErrQueueFull)
	go func() {
		_, c, e := peer.Accept(ctx)
		if e == nil {
			defer c.Close()
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

func TestTCPUDPFramesRoundTripAndMalformedStreamIsolation(t *testing.T) {
	s, peer, ctx := tcpPair(t, RateLimits{}, 0, 0)
	server := make(chan DatagramConn, 1)
	done := make(chan error, 1)
	go func() {
		h, c, e := peer.Accept(ctx)
		if e == nil && (h.Kind != StreamUDP || h.Token != "0123456789abcdef0123456789abcdef") {
			e = ErrProtocol
		}
		if e == nil {
			e = WriteStreamAck(c, nil)
		}
		if e == nil {
			server <- NewDatagramConn(c)
		}
		done <- e
	}()
	client, err := s.OpenUDP(ctx, "0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	defer client.Close()
	require.NoError(t, waitError(t, done))
	remote := <-server
	defer remote.Close()
	for _, frame := range []struct {
		payload []byte
		addr    string
	}{{[]byte("one"), "127.0.0.1:10"}, {make([]byte, 4096), strings.Repeat("a", 256)}} {
		require.NoError(t, client.Send(frame.payload, frame.addr))
		p, a, e := remote.Receive()
		require.NoError(t, e)
		require.Equal(t, frame.payload, p)
		require.Equal(t, frame.addr, a)
		require.NoError(t, remote.Send([]byte("reply"), a))
		p, a, e = client.Receive()
		require.NoError(t, e)
		require.Equal(t, []byte("reply"), p)
		require.Equal(t, frame.addr, a)
	}
	// Bypass only the outbound validator to inject an invalid peer frame.
	_, err = client.(*tcpDatagram).conn.Write([]byte{0x40, 0, 0, 0})
	require.NoError(t, err)
	_, _, err = remote.Receive()
	require.ErrorIs(t, err, ErrProtocol)
	require.ErrorIs(t, remote.Send(nil, ""), net.ErrClosed)
	go func() {
		_, c, e := peer.Accept(ctx)
		if e == nil {
			defer c.Close()
			e = WriteStreamAck(c, nil)
		}
		done <- e
	}()
	control, err := s.OpenControl(ctx)
	require.NoError(t, err)
	defer control.Close()
	require.NoError(t, waitError(t, done))
}

type writeStartedConn struct {
	net.Conn
	started chan struct{}
}

func (c *writeStartedConn) Write(p []byte) (int, error) {
	select {
	case <-c.started:
	default:
		close(c.started)
	}
	return c.Conn.Write(p)
}

func TestTCPUDPQueueSessionClose(t *testing.T) {
	s, peer, ctx := tcpPair(t, RateLimits{}, 0, 0)
	done := make(chan error, 1)
	go func() {
		_, c, err := peer.Accept(ctx)
		if err == nil {
			err = WriteStreamAck(c, nil)
		}
		done <- err
	}()
	d, err := s.OpenUDP(ctx, "0123456789abcdef0123456789abcdef")
	require.NoError(t, err)
	require.NoError(t, waitError(t, done))
	require.NoError(t, s.Close())
	require.Eventually(t, func() bool { return d.Send(nil, "") != nil }, time.Second, time.Millisecond)
	_, err = s.OpenControl(context.Background())
	require.Error(t, err)
}
