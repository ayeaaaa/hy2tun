package client

import (
	"net"
	"strconv"
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
	hopFactory := factory.(*hopConnFactory)
	require.Equal(t, []byte("obfs-secret"), hopFactory.psk)

	conn, err := factory.New(nil)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
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

	entries := cl.logs.Entries("warn", 10)
	require.NotEmpty(t, entries)
	require.Contains(t, entries[len(entries)-1].Message, "port hopping")
}

func TestNewAcceptsHopWhitespace(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Server = "127.0.0.1:30000, 30001"
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, cl.Close()) }()
	require.IsType(t, &udphop.UDPHopAddr{}, cl.serverAddr)
}
