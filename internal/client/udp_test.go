package client

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/testutil"
)

type testUDPMessage struct {
	data []byte
	addr string
}

// fakeUDPConn is an in-memory hyclient.HyUDPConn. incoming feeds Receive and
// sent records every Send call.
type fakeUDPConn struct {
	incoming  chan testUDPMessage
	sent      chan testUDPMessage
	closed    chan struct{}
	closeOnce sync.Once
	sendFn    func()
}

func newFakeUDPConn() *fakeUDPConn {
	return &fakeUDPConn{
		incoming: make(chan testUDPMessage, 32),
		sent:     make(chan testUDPMessage, 32),
		closed:   make(chan struct{}),
	}
}

func (f *fakeUDPConn) Receive() ([]byte, string, error) {
	select {
	case msg := <-f.incoming:
		return msg.data, msg.addr, nil
	case <-f.closed:
		return nil, "", io.EOF
	}
}

func (f *fakeUDPConn) Send(data []byte, addr string) error {
	if f.sendFn != nil {
		f.sendFn()
	}
	msg := testUDPMessage{data: append([]byte(nil), data...), addr: addr}
	select {
	case f.sent <- msg:
		return nil
	case <-f.closed:
		return io.EOF
	}
}

func (f *fakeUDPConn) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeUDPConn) isClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}

// fakeUDPHY2Client supplies one fake UDP session and is otherwise unused.
type fakeUDPHY2Client struct {
	udp hyclient.HyUDPConn
}

func (f *fakeUDPHY2Client) TCP(string) (net.Conn, error) {
	return nil, fmt.Errorf("TCP is not used by this test")
}

func (f *fakeUDPHY2Client) UDP() (hyclient.HyUDPConn, error) {
	return f.udp, nil
}

func (f *fakeUDPHY2Client) Close() error {
	return nil
}

func TestUDPMappingForwardsRepliesAndClosesPeers(t *testing.T) {
	echoAddr, stopEcho := testutil.StartUDPEcho(t)
	defer stopEcho()

	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()

	fake := newFakeUDPConn()
	cl.mu.Lock()
	cl.hy = &fakeUDPHY2Client{udp: fake}
	cl.mu.Unlock()

	token := strings.Repeat("a", 32)
	u, err := cl.startUDP(config.Mapping{
		ID:        "udp1",
		Type:      "udp",
		LocalAddr: echoAddr,
		Enabled:   true,
	}, token)
	require.NoError(t, err)
	defer u.close()

	probe := <-fake.sent
	require.Equal(t, []byte{0}, probe.data)
	require.Equal(t, udpVirtualAddress(token), probe.addr)
	require.Equal(t, udpKeepaliveInterval, u.keepaliveInterval)

	fake.incoming <- testUDPMessage{data: []byte("hello"), addr: "203.0.113.7:5555"}
	select {
	case reply := <-fake.sent:
		require.Equal(t, []byte("hello"), reply.data)
		require.Equal(t, "203.0.113.7:5555", reply.addr)
	case <-time.After(3 * time.Second):
		t.Fatal("no reply from the local echo target")
	}
	require.Equal(t, 1, u.peerCount())

	u.close()
	require.Equal(t, 0, u.peerCount())
	require.True(t, fake.isClosed(), "closing the mapping must close the HY2 UDP session")
}

func TestStartUDPMappingsUsesRegistrationTokens(t *testing.T) {
	echoAddr, stopEcho := testutil.StartUDPEcho(t)
	defer stopEcho()

	cfg := validTestClientConfig()
	cfg.Mappings = []config.Mapping{
		{ID: "udp1", Type: "udp", LocalAddr: echoAddr, Enabled: true},
	}
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer cl.Close()

	fake := newFakeUDPConn()
	token := strings.Repeat("b", 32)
	cl.mu.Lock()
	cl.hy = &fakeUDPHY2Client{udp: fake}
	cl.udpTokens = map[string]string{"udp1": token}
	cl.mu.Unlock()

	cl.startUDPMappings()
	require.Len(t, cl.udpMappings, 1)
	probe := <-fake.sent
	require.Equal(t, udpVirtualAddress(token), probe.addr)

	cl.stopUDPMappings()
	require.Empty(t, cl.udpMappings)
	require.True(t, fake.isClosed())
}

func TestStartUDPRejectsMalformedToken(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	defer cl.Close()

	_, err = cl.startUDP(config.Mapping{
		ID:        "udp1",
		Type:      "udp",
		LocalAddr: "127.0.0.1:9",
		Enabled:   true,
	}, "not-a-token")
	require.Error(t, err)
}

func TestUDPMappingSendIsSerialized(t *testing.T) {
	fake := newFakeUDPConn()
	var inFlight, overlaps atomic.Int32
	fake.sendFn = func() {
		if inFlight.Add(1) > 1 {
			overlaps.Add(1)
		}
		time.Sleep(time.Millisecond)
		inFlight.Add(-1)
	}

	u := &udpMapping{
		id:    "udp1",
		conn:  fake,
		log:   slog.Default(),
		peers: make(map[string]*udpPeer),
	}

	const senders = 16
	errCh := make(chan error, senders)
	var wg sync.WaitGroup
	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- u.send([]byte("x"), "203.0.113.1:1")
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	require.Zero(t, overlaps.Load(), "HyUDPConn.Send must never run concurrently")
	require.Equal(t, senders, len(fake.sent))
}

func TestUDPMappingDropsOversizedDatagram(t *testing.T) {
	fake := newFakeUDPConn()
	u := &udpMapping{
		id:    "udp1",
		conn:  fake,
		log:   slog.Default(),
		peers: make(map[string]*udpPeer),
	}
	require.NoError(t, u.send(make([]byte, udpMaxDatagram+1), "203.0.113.1:1"))
	require.Equal(t, 0, len(fake.sent), "an oversized datagram must not reach the core")
}

func TestUDPMappingPeerCapDropsOldest(t *testing.T) {
	u := newTestUDPMapping(t)
	defer u.close()
	u.maxPeers = 2

	p1, err := u.peer("203.0.113.1:1")
	require.NoError(t, err)
	_, err = u.peer("203.0.113.2:2")
	require.NoError(t, err)
	p1.last.Store(time.Now().Add(-time.Hour).UnixNano())

	_, err = u.peer("203.0.113.3:3")
	require.NoError(t, err)
	require.Equal(t, 2, u.peerCount())

	u.mu.Lock()
	_, hasOldest := u.peers["203.0.113.1:1"]
	_, hasSecond := u.peers["203.0.113.2:2"]
	_, hasThird := u.peers["203.0.113.3:3"]
	u.mu.Unlock()
	require.False(t, hasOldest, "the oldest peer must be dropped at the cap")
	require.True(t, hasSecond)
	require.True(t, hasThird)
}

func TestUDPMappingExpiresIdlePeers(t *testing.T) {
	u := newTestUDPMapping(t)
	defer u.close()

	p1, err := u.peer("203.0.113.1:1")
	require.NoError(t, err)
	_, err = u.peer("203.0.113.2:2")
	require.NoError(t, err)
	p1.last.Store(time.Now().Add(-2 * time.Minute).UnixNano())

	u.expirePeers(time.Now())
	require.Equal(t, 1, u.peerCount())

	u.mu.Lock()
	_, hasIdle := u.peers["203.0.113.1:1"]
	u.mu.Unlock()
	require.False(t, hasIdle)
}

func TestUDPMappingKeepaliveSendsProbes(t *testing.T) {
	fake := newFakeUDPConn()
	defer fake.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u := &udpMapping{
		id:                "udp1",
		virtualAddr:       "_hy2tun_udp_token:0",
		conn:              fake,
		log:               slog.Default(),
		peers:             make(map[string]*udpPeer),
		keepaliveInterval: 5 * time.Millisecond,
	}

	done := make(chan struct{})
	go func() {
		u.keepalive(ctx)
		close(done)
	}()
	for i := 0; i < 2; i++ {
		select {
		case msg := <-fake.sent:
			require.Equal(t, []byte{0}, msg.data)
			require.Equal(t, u.virtualAddr, msg.addr)
		case <-time.After(time.Second):
			t.Fatal("keepalive probe was not sent")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("keepalive did not stop after cancellation")
	}
}

func newTestUDPMapping(t *testing.T) *udpMapping {
	t.Helper()

	_, cancel := context.WithCancel(context.Background())
	return &udpMapping{
		id:     "udp1",
		log:    slog.Default(),
		cancel: cancel,
		peers:  make(map[string]*udpPeer),
		target: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9},
	}
}
