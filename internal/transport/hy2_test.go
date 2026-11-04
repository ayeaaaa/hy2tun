package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/quic-go"
	"github.com/stretchr/testify/require"
)

// The fake replaces only HY2 network operations. Literal virtual addresses and
// real net.Pipe endpoints catch wrong routing and missing connection cleanup.
func TestHY2AdapterVirtualAddresses(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  TCPRequest
		want string
	}{
		{"control", TCPRequest{}, "_hy2tun_control_:0"},
		{"reverse", TCPRequest{ReverseToken: "0123456789abcdef0123456789abcdef"}, "_hy2tun_data_0123456789abcdef0123456789abcdef:0"},
		{"proxy", TCPRequest{Target: "example.test:8080"}, "example.test:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, remote := net.Pipe()
			defer remote.Close()
			fake := newFakeHY2()
			fake.tcp = func(addr string) (net.Conn, error) {
				if addr != tc.want {
					return nil, errors.New("incorrect virtual address")
				}
				return local, nil
			}
			s := WrapHY2(fake, "control-secret")
			defer s.Close()
			require.Equal(t, HY2, s.Kind())
			var conn net.Conn
			var err error
			if tc.name == "control" {
				conn, err = s.OpenControl(context.Background())
			} else {
				conn, err = s.OpenTCP(context.Background(), tc.req)
			}
			require.NoError(t, err)
			require.NotNil(t, conn)
			require.NoError(t, conn.Close())
			requirePeerClosed(t, remote)
		})
	}
	t.Run("UDP mapping registration and peer addresses", func(t *testing.T) {
		udp := newFakeDatagram()
		fake := newFakeHY2()
		fake.udp = func() (hyclient.HyUDPConn, error) { return udp, nil }
		s := WrapHY2(fake, "control-secret")
		defer s.Close()
		conn, err := s.OpenUDP(context.Background(), "0123456789abcdef0123456789abcdef")
		require.NoError(t, err)
		require.Equal(t, datagram{[]byte{0}, "_hy2tun_udp_0123456789abcdef0123456789abcdef:0"}, waitDatagram(t, udp.sent))
		require.NoError(t, conn.Send([]byte("reply"), "198.51.100.1:1234"))
		require.Equal(t, datagram{[]byte("reply"), "198.51.100.1:1234"}, waitDatagram(t, udp.sent))
		udp.incoming <- datagram{[]byte("request"), "198.51.100.1:1234"}
		data, addr, err := conn.Receive()
		require.NoError(t, err)
		require.Equal(t, []byte("request"), data)
		require.Equal(t, "198.51.100.1:1234", addr)
		require.NoError(t, conn.Close())
		waitSignal(t, udp.closed)
	})
}

func TestHY2AdapterRejectsInvalidRequests(t *testing.T) {
	fake := newFakeHY2() // Unconfigured operations fail if validation is skipped.
	s := WrapHY2(fake, "secret")
	defer s.Close()
	for _, req := range []TCPRequest{
		{},
		{ReverseToken: "0123456789abcdef0123456789abcdef", Target: "example.test:80"},
		{ReverseToken: "bad:token"},
	} {
		conn, err := s.OpenTCP(context.Background(), req)
		require.Nil(t, conn)
		require.ErrorIs(t, err, ErrProtocol)
		require.False(t, FallbackAllowed(err))
	}
	conn, err := s.OpenUDP(context.Background(), "")
	require.Nil(t, conn)
	require.ErrorIs(t, err, ErrProtocol)
	_, err = s.Probe(context.Background())
	require.ErrorIs(t, err, ErrProtocol) // Must not call TCP/Hello for Task 2.
}

func TestHY2AdapterCancellationClosesLateTCP(t *testing.T) {
	for _, operation := range []string{"control", "reverse", "proxy"} {
		for _, shutdown := range []string{"cancel", "close"} {
			t.Run(operation+"/"+shutdown, func(t *testing.T) {
				local, remote := net.Pipe()
				defer remote.Close()
				fake := newFakeHY2()
				started, release := make(chan struct{}), make(chan struct{})
				fake.tcp = func(string) (net.Conn, error) {
					close(started)
					<-release
					return local, nil
				}
				s := WrapHY2(fake, "secret")
				defer s.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					var conn net.Conn
					var err error
					switch operation {
					case "control":
						conn, err = s.OpenControl(ctx)
					case "reverse":
						conn, err = s.OpenTCP(ctx, TCPRequest{ReverseToken: "0123456789abcdef0123456789abcdef"})
					default:
						conn, err = s.OpenTCP(ctx, TCPRequest{Target: "example.test:80"})
					}
					if conn != nil {
						_ = conn.Close()
						done <- errors.New("delivered a cancelled connection")
						return
					}
					done <- err
				}()
				waitSignal(t, started)
				if shutdown == "cancel" {
					cancel()
					require.ErrorIs(t, waitError(t, done), context.Canceled)
				} else {
					require.NoError(t, s.Close())
					require.ErrorIs(t, waitError(t, done), net.ErrClosed)
				}
				waitSignal(t, fake.closed)
				close(release)
				requirePeerClosed(t, remote)
			})
		}
	}
}

func TestHY2AdapterCancellationClosesLateUDP(t *testing.T) {
	udp := newFakeDatagram()
	fake := newFakeHY2()
	started, release := make(chan struct{}), make(chan struct{})
	fake.udp = func() (hyclient.HyUDPConn, error) {
		close(started)
		<-release
		return udp, nil
	}
	s := WrapHY2(fake, "secret")
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.OpenUDP(ctx, "0123456789abcdef0123456789abcdef"); done <- err }()
	waitSignal(t, started)
	cancel()
	require.ErrorIs(t, waitError(t, done), context.Canceled)
	waitSignal(t, fake.closed)
	close(release)
	waitSignal(t, udp.closed)
}

func TestHY2AdapterShutdownClosesBlockedUDPRegistration(t *testing.T) {
	// UDP() has already returned this child. Root Close deliberately does not
	// propagate to it, so the adapter must close it while registration blocks.
	// A real pipe write proves the operation is released by child closure.
	for _, shutdown := range []string{"cancel", "close"} {
		t.Run(shutdown, func(t *testing.T) {
			local, remote := net.Pipe()
			defer remote.Close()
			udp := &blockedRegistrationDatagram{conn: local, started: make(chan struct{}), finished: make(chan struct{})}
			defer udp.Close() // Release a broken implementation even on RED.
			fake := newFakeHY2()
			fake.udp = func() (hyclient.HyUDPConn, error) { return udp, nil }
			s := WrapHY2(fake, "secret")
			defer s.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				conn, err := s.OpenUDP(ctx, "0123456789abcdef0123456789abcdef")
				if conn != nil {
					_ = conn.Close()
					done <- errors.New("delivered an unregistered UDP connection")
					return
				}
				done <- err
			}()
			waitSignal(t, udp.started)
			if shutdown == "cancel" {
				cancel()
				require.ErrorIs(t, waitError(t, done), context.Canceled)
			} else {
				require.NoError(t, s.Close())
				require.ErrorIs(t, waitError(t, done), net.ErrClosed)
			}
			waitSignal(t, fake.closed)
			// Wait before reading the peer: a peer read would otherwise release
			// the blocked write and conceal failure to close the UDP child.
			waitSignal(t, udp.finished)
			requirePeerClosed(t, remote)
		})
	}
}

func TestHY2AdapterClosesConnectionsReturnedWithErrors(t *testing.T) {
	local, remote := net.Pipe()
	defer remote.Close()
	fake := newFakeHY2()
	fake.tcp = func(string) (net.Conn, error) { return local, io.EOF }
	udp := newFakeDatagram()
	fake.udp = func() (hyclient.HyUDPConn, error) { return udp, io.EOF }
	s := WrapHY2(fake, "secret")
	defer s.Close()
	conn, err := s.OpenControl(context.Background())
	require.Nil(t, conn)
	require.ErrorIs(t, err, io.EOF)
	requirePeerClosed(t, remote)
	udpConn, err := s.OpenUDP(context.Background(), "0123456789abcdef0123456789abcdef")
	require.Nil(t, udpConn)
	require.ErrorIs(t, err, io.EOF)
	waitSignal(t, udp.closed)
}

func TestDialHY2CancellationClosesPacketConnAndLateClient(t *testing.T) {
	// HY2 creates its packet socket before returning. Cancelling just the
	// factory-result wait would leave the handshake and its socket alive.
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer packet.Close()
	started, release := make(chan struct{}), make(chan struct{})
	fake := newFakeHY2()
	cfg := &hyclient.Config{ConnFactory: packetFactory{packet}, ServerAddr: packet.LocalAddr()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := DialHY2(ctx, DialOptions{HY2: cfg}, func(cfg *hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error) {
			if _, err := cfg.ConnFactory.New(cfg.ServerAddr); err != nil {
				return nil, nil, err
			}
			close(started)
			<-release
			return fake, nil, nil
		})
		done <- err
	}()
	waitSignal(t, started)
	cancel()
	require.ErrorIs(t, waitError(t, done), context.Canceled)
	_ = packet.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = packet.ReadFrom(make([]byte, 1))
	require.ErrorIs(t, err, net.ErrClosed)
	close(release)
	waitSignal(t, fake.closed)
}

func TestDialHY2OwnsPacketConnWithoutMutatingConfig(t *testing.T) {
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer packet.Close()
	fake := newFakeHY2()
	originalFactory := packetFactory{packet}
	cfg := &hyclient.Config{ConnFactory: originalFactory, Auth: "hy2-auth", BandwidthConfig: hyclient.BandwidthConfig{MaxTx: 123, MaxRx: 456}}
	s, err := DialHY2(context.Background(), DialOptions{HY2: cfg, ControlToken: "secret"}, func(got *hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error) {
		if got.Auth != "hy2-auth" || got.BandwidthConfig.MaxTx != 123 || got.BandwidthConfig.MaxRx != 456 {
			return nil, nil, errors.New("lost HY2 configuration")
		}
		_, err := got.ConnFactory.New(nil)
		return fake, &hyclient.HandshakeInfo{UDPEnabled: true, Tx: 123}, err
	})
	require.NoError(t, err)
	require.Equal(t, originalFactory, cfg.ConnFactory)
	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	waitSignal(t, fake.closed)
	// Deadline makes a missed close fail instead of hanging the test suite.
	_ = packet.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = packet.ReadFrom(make([]byte, 1))
	require.ErrorIs(t, err, net.ErrClosed)
	_, err = s.OpenControl(context.Background())
	require.ErrorIs(t, err, net.ErrClosed)
}

func TestHY2PreCancelledCallsHaveNoSideEffects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	factory := func(*hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error) {
		t.Error("cancelled dial invoked factory")
		return nil, nil, nil
	}
	_, err := DialHY2(ctx, DialOptions{HY2: &hyclient.Config{}}, factory)
	require.ErrorIs(t, err, context.Canceled)
	fake := newFakeHY2()
	s := WrapHY2(fake, "secret")
	defer s.Close()
	_, err = s.OpenControl(ctx)
	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-fake.closed:
		t.Fatal("pre-cancelled operation closed an existing session")
	default:
	}
}

func TestDialHY2PreservesPacketSocketCapabilities(t *testing.T) {
	// Hiding the UDP socket's OOB interface silently disables QUIC's ECN,
	// batching, buffer tuning and GSO. Exercise it on a real loopback socket.
	packet, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer packet.Close()
	receiver, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer receiver.Close()
	s, err := DialHY2(context.Background(), DialOptions{HY2: &hyclient.Config{ConnFactory: packetFactory{packet}}}, func(cfg *hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error) {
		conn, err := cfg.ConnFactory.New(nil)
		if err != nil {
			return nil, nil, err
		}
		oob, ok := conn.(quic.OOBCapablePacketConn)
		if !ok {
			return nil, nil, errors.New("packet tracking hid QUIC socket capabilities")
		}
		_, _, err = oob.WriteMsgUDP([]byte("packet"), nil, receiver.LocalAddr().(*net.UDPAddr))
		return newFakeHY2(), nil, err
	})
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, receiver.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 32)
	n, _, err := receiver.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "packet", string(buf[:n]))
}

type fakeHY2 struct {
	tcp       func(string) (net.Conn, error)
	udp       func() (hyclient.HyUDPConn, error)
	closed    chan struct{}
	closeOnce sync.Once
}

func newFakeHY2() *fakeHY2 { return &fakeHY2{closed: make(chan struct{})} }
func (f *fakeHY2) TCP(addr string) (net.Conn, error) {
	if f.tcp == nil {
		return nil, errors.New("unexpected TCP operation")
	}
	return f.tcp(addr)
}
func (f *fakeHY2) UDP() (hyclient.HyUDPConn, error) {
	if f.udp == nil {
		return nil, errors.New("unexpected UDP operation")
	}
	return f.udp()
}
func (f *fakeHY2) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

type datagram struct {
	data []byte
	addr string
}
type fakeDatagram struct {
	sent, incoming chan datagram
	closed         chan struct{}
	closeOnce      sync.Once
}

func newFakeDatagram() *fakeDatagram {
	return &fakeDatagram{sent: make(chan datagram, 4), incoming: make(chan datagram, 4), closed: make(chan struct{})}
}
func (f *fakeDatagram) Send(data []byte, addr string) error {
	f.sent <- datagram{append([]byte(nil), data...), addr}
	return nil
}
func (f *fakeDatagram) Receive() ([]byte, string, error) {
	select {
	case d := <-f.incoming:
		return d.data, d.addr, nil
	case <-f.closed:
		return nil, "", net.ErrClosed
	}
}
func (f *fakeDatagram) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

type blockedRegistrationDatagram struct {
	conn              net.Conn
	started, finished chan struct{}
}

func (d *blockedRegistrationDatagram) Receive() ([]byte, string, error) {
	return nil, "", errors.New("unexpected Receive during UDP registration")
}
func (d *blockedRegistrationDatagram) Send(data []byte, addr string) error {
	if len(data) != 1 || data[0] != 0 || addr != "_hy2tun_udp_0123456789abcdef0123456789abcdef:0" {
		return errors.New("incorrect reverse UDP registration")
	}
	close(d.started)
	defer close(d.finished)
	_, err := d.conn.Write(data)
	return err
}
func (d *blockedRegistrationDatagram) Close() error { return d.conn.Close() }

type packetFactory struct{ conn net.PacketConn }

func (f packetFactory) New(net.Addr) (net.PacketConn, error) { return f.conn, nil }

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("operation did not finish")
	}
}
func waitError(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("operation did not finish")
		return nil
	}
}
func waitDatagram(t *testing.T, ch <-chan datagram) datagram {
	t.Helper()
	select {
	case d := <-ch:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("expected datagram was not sent")
		return datagram{}
	}
}
func requirePeerClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := conn.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
}
