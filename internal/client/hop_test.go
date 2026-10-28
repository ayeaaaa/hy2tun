package client

import (
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apernet/hysteria/extras/v2/transport/udphop"
	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
)

func TestResolveServerAddrSinglePort(t *testing.T) {
	cfg := validTestClientConfig()
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)
	require.IsType(t, &net.UDPAddr{}, addr)
}

func TestResolveServerAddrPortHopping(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:30000-30001"
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)
	require.IsType(t, &udphop.UDPHopAddr{}, addr)
	require.Equal(t, "udphop", addr.Network())
}

func TestResolveServerAddrNormalizesOnePortSet(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:443,443"
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)
	require.IsType(t, &net.UDPAddr{}, addr)
	require.Equal(t, 443, addr.(*net.UDPAddr).Port)
}

func TestResolveServerAddrNormalizesHopWhitespace(t *testing.T) {
	servers := []string{
		"127.0.0.1:30000, 30001",
		"127.0.0.1:30000 - 30001",
		" 127.0.0.1:30000, 30001 ",
	}
	for _, server := range servers {
		t.Run(server, func(t *testing.T) {
			cfg := validTestClientConfig()
			cfg.Server = server
			addr, err := resolveServerAddr(cfg)
			require.NoError(t, err)
			hopAddr, ok := addr.(*udphop.UDPHopAddr)
			require.True(t, ok)
			require.Equal(t, []uint16{30000, 30001}, hopAddr.Ports)
		})
	}
	t.Run("whitespace-only host", func(t *testing.T) {
		cfg := validTestClientConfig()
		cfg.Server = " :443,8443"
		addr, err := resolveServerAddr(cfg)
		require.Nil(t, addr)
		require.ErrorContains(t, err, "server must include a host")
	})
}

func TestBuildConnFactorySelectsHop(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:30000-30001"
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)

	factory, err := buildConnFactory(cfg, addr)
	require.NoError(t, err)
	require.IsType(t, &hopConnFactory{}, factory)
}

func TestBuildConnFactorySinglePortUnchanged(t *testing.T) {
	cfg := validTestClientConfig()
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)

	factory, err := buildConnFactory(cfg, addr)
	require.NoError(t, err)
	require.Nil(t, factory)

	cfg.Obfs.Type = "salamander"
	cfg.Obfs.Salamander.Password = "obfs-secret"
	factory, err = buildConnFactory(cfg, addr)
	require.NoError(t, err)
	require.IsType(t, &salamanderConnFactory{}, factory)
}

func TestHopConnFactoryCreatesAndCloses(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:30000-30001"
	cfg.Transport = &config.ClientTransportConfig{UDP: &config.ClientTransportUDPConfig{
		HopInterval: config.Duration(5 * time.Second),
	}}
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)

	factory, err := buildConnFactory(cfg, addr)
	require.NoError(t, err)
	conn, err := factory.New(nil)
	require.NoError(t, err)
	require.NotNil(t, conn)
	require.NoError(t, conn.Close())
}

func TestHopConnFactoryWithObfs(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:30000-30001"
	cfg.Obfs.Type = "salamander"
	cfg.Obfs.Salamander.Password = "obfs-secret"
	addr, err := resolveServerAddr(cfg)
	require.NoError(t, err)

	factory, err := buildConnFactory(cfg, addr)
	require.NoError(t, err)
	require.IsType(t, &hopConnFactory{}, factory)
	hopFactory := factory.(*hopConnFactory)
	require.Equal(t, []byte("obfs-secret"), hopFactory.psk)

	conn, err := factory.New(nil)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestHopConnFactoryInterval(t *testing.T) {
	tests := []struct {
		name string
		udp  *config.ClientTransportUDPConfig
		want udphop.HopIntervalConfig
	}{
		{
			name: "configured 5s",
			udp:  &config.ClientTransportUDPConfig{HopInterval: config.Duration(5 * time.Second)},
			want: udphop.HopIntervalConfig{Min: 5 * time.Second, Max: 5 * time.Second},
		},
		{
			name: "default zero values",
			want: udphop.HopIntervalConfig{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validTestClientConfig()
			cfg.Server = "127.0.0.1:30000-30001"
			if tt.udp != nil {
				cfg.Transport = &config.ClientTransportConfig{UDP: tt.udp}
			}
			addr, err := resolveServerAddr(cfg)
			require.NoError(t, err)
			factory, err := buildConnFactory(cfg, addr)
			require.NoError(t, err)
			require.IsType(t, &hopConnFactory{}, factory)
			hopFactory := factory.(*hopConnFactory)
			require.Equal(t, tt.want, hopFactory.interval)
		})
	}
}

func TestHopConnFactoryObfsWrapsPackets(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer func() { require.NoError(t, listener.Close()) }()
	port := listener.LocalAddr().(*net.UDPAddr).Port

	addr := &udphop.UDPHopAddr{
		IP:      net.IPv4(127, 0, 0, 1),
		Ports:   []uint16{uint16(port)},
		PortStr: strconv.Itoa(port),
	}
	factory := &hopConnFactory{
		addr:     addr,
		interval: udphop.HopIntervalConfig{Min: 5 * time.Second, Max: 5 * time.Second},
		psk:      []byte("obfs-secret"),
	}
	conn, err := factory.New(nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()

	payload := []byte("hello-port-hop")
	_, err = conn.WriteTo(payload, nil)
	require.NoError(t, err)

	require.NoError(t, listener.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 2048)
	n, _, err := listener.ReadFromUDP(buf)
	require.NoError(t, err)
	require.NotEqual(t, string(payload), string(buf[:n]))
}

// recordingPacketConn is a net.PacketConn that records writes instead of
// touching the network. It lets the hop test observe the bytes handed to every
// socket opened by udphop.
type recordingPacketConn struct {
	mu     sync.Mutex
	writes [][]byte
	closed chan struct{}
	once   sync.Once
}

func newRecordingPacketConn() *recordingPacketConn {
	return &recordingPacketConn{closed: make(chan struct{})}
}

func (c *recordingPacketConn) ReadFrom([]byte) (int, net.Addr, error) {
	<-c.closed
	return 0, nil, net.ErrClosed
}

func (c *recordingPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	c.mu.Lock()
	c.writes = append(c.writes, append([]byte(nil), p...))
	c.mu.Unlock()
	return len(p), nil
}

func (c *recordingPacketConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *recordingPacketConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}

func (c *recordingPacketConn) SetDeadline(time.Time) error      { return nil }
func (c *recordingPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *recordingPacketConn) SetWriteDeadline(time.Time) error { return nil }

func (c *recordingPacketConn) written() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.writes))
	for i, write := range c.writes {
		out[i] = append([]byte(nil), write...)
	}
	return out
}

// TestHopConnFactoryObfsWrapsEveryHopSocket forces one hop and verifies that
// both the initial socket and the socket opened after the hop receive
// obfuscated bytes. The listenUDP seam makes this deterministic without
// touching the network.
func TestHopConnFactoryObfsWrapsEveryHopSocket(t *testing.T) {
	addr := &udphop.UDPHopAddr{
		IP:      net.IPv4(127, 0, 0, 1),
		Ports:   []uint16{30000, 30001},
		PortStr: "30000-30001",
	}
	opened := make(chan *recordingPacketConn, 4)
	factory := &hopConnFactory{
		addr:     addr,
		interval: udphop.HopIntervalConfig{Min: 5 * time.Second, Max: 5 * time.Second},
		psk:      []byte("obfs-secret"),
		listenUDP: func() (net.PacketConn, error) {
			conn := newRecordingPacketConn()
			opened <- conn
			return conn, nil
		},
	}
	conn, err := factory.New(nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, conn.Close()) }()

	payload := []byte("hello-port-hop")
	assertWrapped := func(socket *recordingPacketConn) {
		writes := socket.written()
		require.Len(t, writes, 1)
		require.NotEmpty(t, writes[0])
		require.NotEqual(t, string(payload), string(writes[0]))
	}

	first := <-opened
	_, err = conn.WriteTo(payload, nil)
	require.NoError(t, err)
	assertWrapped(first)

	var second *recordingPacketConn
	select {
	case second = <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("hop did not open a second socket")
	}

	// WriteTo blocks on the hop's write lock until currentConn is the new
	// socket, so this write must land on the post-hop connection.
	_, err = conn.WriteTo(payload, nil)
	require.NoError(t, err)
	assertWrapped(second)
}

func TestBuildTLSConfigUsesHostFromPortHoppingAddress(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "example.com:30000-30001"
	tlsConfig, err := buildTLSConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, "example.com", tlsConfig.ServerName)
}

func TestBuildTLSConfigTrimsHostWhitespace(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = " example.com:30000, 30001 "
	tlsConfig, err := buildTLSConfig(cfg)
	require.NoError(t, err)
	require.Equal(t, "example.com", tlsConfig.ServerName)
}

func TestNewWarnsForPortHopping(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:30000-30001"
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, cl.Close()) }()

	warning := portHoppingWarning(t, cl)
	require.Contains(t, warning, "hop_min_interval=30s")
	require.Contains(t, warning, "hop_max_interval=30s")
}

func TestNewWarnsWithConfiguredHopInterval(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:30000-30001"
	cfg.Transport = &config.ClientTransportConfig{UDP: &config.ClientTransportUDPConfig{
		HopInterval: config.Duration(5 * time.Second),
	}}
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, cl.Close()) }()

	warning := portHoppingWarning(t, cl)
	require.Contains(t, warning, "hop_min_interval=5s")
	require.Contains(t, warning, "hop_max_interval=5s")
}

func TestNewDoesNotWarnForSinglePort(t *testing.T) {
	cfg := validTestClientConfig()
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, cl.Close()) }()

	for _, entry := range cl.logs.Entries("warn", 10) {
		require.NotContains(t, entry.Message, "port hopping")
	}
}

func portHoppingWarning(t *testing.T, cl *Client) string {
	t.Helper()
	for _, entry := range cl.logs.Entries("warn", 10) {
		if strings.Contains(entry.Message, "port hopping") {
			return entry.Message
		}
	}
	t.Fatal("port hopping warning not found")
	return ""
}

func TestNewAcceptsHopWhitespace(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:30000, 30001"
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, cl.Close()) }()
	require.IsType(t, &udphop.UDPHopAddr{}, cl.serverAddr)
}
