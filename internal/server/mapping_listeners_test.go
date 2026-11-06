package server

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/view"
)

// requireIPv6Loopback skips a test only when the host cannot bind either an
// IPv6 TCP or UDP loopback socket. The reason is recorded in the skip message.
func requireIPv6Loopback(t *testing.T) {
	t.Helper()

	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("host lacks IPv6 TCP loopback support: %v", err)
	}
	_ = listener.Close()

	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("host lacks IPv6 UDP loopback support: %v", err)
	}
	_ = conn.Close()
}

// nonLoopbackIPv4 returns an address that is not served by a loopback-only
// listener. When the host has no non-loopback IPv4 address, 127.0.0.2 is used;
// on Linux the entire 127.0.0.0/8 block is loopback, so a socket bound only to
// 127.0.0.1 must refuse it.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()

	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			return ip.String()
		}
	}
	t.Log("no non-loopback IPv4 address; using 127.0.0.2 to prove the listener is not wildcard-bound")
	return "127.0.0.2"
}

func acceptOne(t *testing.T, listener net.Listener) <-chan net.Conn {
	t.Helper()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()
	return accepted
}

func dialAndAccept(t *testing.T, listener net.Listener, network, addr string) {
	t.Helper()

	accepted := acceptOne(t, listener)
	conn, err := net.DialTimeout(network, addr, 2*time.Second)
	require.NoError(t, err, "dial %s", addr)
	defer conn.Close()

	select {
	case serverConn, ok := <-accepted:
		if !ok {
			t.Fatalf("listener %s did not accept %s", listener.Addr(), addr)
		}
		_ = serverConn.Close()
	case <-time.After(2 * time.Second):
		t.Fatalf("listener %s did not accept %s", listener.Addr(), addr)
	}
}

func requireTCPPortReleased(t *testing.T, network, addr string) {
	t.Helper()

	listener, err := net.Listen(network, addr)
	require.NoError(t, err, "rebind %s after close", addr)
	require.NoError(t, listener.Close())
}

func requireUDPPortReleased(t *testing.T, network string, ip net.IP, port int) {
	t.Helper()

	conn, err := net.ListenUDP(network, &net.UDPAddr{IP: ip, Port: port})
	require.NoError(t, err, "rebind %s port %d after close", network, port)
	require.NoError(t, conn.Close())
}

func listenerHost(t *testing.T, listener net.Listener) netip.Addr {
	t.Helper()

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok, "unexpected listener address %T", listener.Addr())
	addr, ok := netip.AddrFromSlice(tcpAddr.IP)
	require.True(t, ok)
	return addr.Unmap()
}

func TestLocalTCPListeners(t *testing.T) {
	requireIPv6Loopback(t)

	set, err := bindMappingListeners(view.ExposureLocal, "192.0.2.10", "tcp", 0)
	require.NoError(t, err)
	require.NotNil(t, set)
	defer set.Close()

	require.Len(t, set.TCP, 2)
	require.Empty(t, set.Warnings())
	require.Positive(t, set.Port())

	hosts := make(map[netip.Addr]bool, len(set.TCP))
	for _, listener := range set.TCP {
		hosts[listenerHost(t, listener)] = true
		require.Equal(t, set.Port(), addrPort(listener.Addr()))
	}
	require.True(t, hosts[netip.MustParseAddr("127.0.0.1")])
	require.True(t, hosts[netip.MustParseAddr("::1")])

	for _, listener := range set.TCP {
		host := listenerHost(t, listener)
		network := "tcp4"
		addr := net.JoinHostPort(host.String(), strconv.Itoa(set.Port()))
		if host.Is6() {
			network = "tcp6"
			addr = net.JoinHostPort(host.String(), strconv.Itoa(set.Port()))
		}
		dialAndAccept(t, listener, network, addr)
	}

	publicAddr := net.JoinHostPort(nonLoopbackIPv4(t), strconv.Itoa(set.Port()))
	conn, err := net.DialTimeout("tcp", publicAddr, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
	}
	require.Error(t, err, "a local mapping must not accept a public address")

	port := set.Port()
	require.NoError(t, set.Close())
	requireTCPPortReleased(t, "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	requireTCPPortReleased(t, "tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
}

func TestPublicDualStack(t *testing.T) {
	requireIPv6Loopback(t)

	set, err := bindMappingListeners(view.ExposurePublic, "", "tcp", 0)
	require.NoError(t, err)
	require.NotNil(t, set)
	defer set.Close()

	require.Len(t, set.TCP, 2)
	require.Empty(t, set.Warnings())
	require.Positive(t, set.Port())

	families := make(map[string]net.Listener, len(set.TCP))
	for _, listener := range set.TCP {
		tcpAddr, ok := listener.Addr().(*net.TCPAddr)
		require.True(t, ok)
		require.True(t, tcpAddr.IP.IsUnspecified(), "public listener %s must be wildcard", listener.Addr())
		if tcpAddr.IP.To4() != nil {
			families["ipv4"] = listener
		} else {
			families["ipv6"] = listener
		}
	}
	require.Contains(t, families, "ipv4")
	require.Contains(t, families, "ipv6")

	port := strconv.Itoa(set.Port())
	dialAndAccept(t, families["ipv4"], "tcp4", net.JoinHostPort("127.0.0.1", port))
	dialAndAccept(t, families["ipv6"], "tcp6", net.JoinHostPort("::1", port))

	require.NoError(t, set.Close())
	requireTCPPortReleased(t, "tcp4", net.JoinHostPort("0.0.0.0", port))
	requireTCPPortReleased(t, "tcp6", net.JoinHostPort("::", port))
}

func TestPartialFamilyWarning(t *testing.T) {
	requireIPv6Loopback(t)

	t.Run("ipv6 unavailable", func(t *testing.T) {
		occupied, err := net.Listen("tcp6", "[::]:0")
		require.NoError(t, err)
		defer occupied.Close()
		port := occupied.Addr().(*net.TCPAddr).Port

		set, err := bindMappingListeners(view.ExposurePublic, "", "tcp", port)
		require.NoError(t, err)
		require.NotNil(t, set)
		defer set.Close()

		require.Len(t, set.TCP, 1)
		require.Equal(t, []string{"ipv6"}, set.Warnings())
		dialAndAccept(t, set.TCP[0], "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	})

	t.Run("ipv4 unavailable", func(t *testing.T) {
		occupied, err := net.Listen("tcp4", "0.0.0.0:0")
		require.NoError(t, err)
		defer occupied.Close()
		port := occupied.Addr().(*net.TCPAddr).Port

		set, err := bindMappingListeners(view.ExposurePublic, "", "tcp", port)
		require.NoError(t, err)
		require.NotNil(t, set)
		defer set.Close()

		require.Len(t, set.TCP, 1)
		require.Equal(t, []string{"ipv4"}, set.Warnings())
		dialAndAccept(t, set.TCP[0], "tcp6", net.JoinHostPort("::1", strconv.Itoa(port)))
	})
}

func TestBothFamiliesFail(t *testing.T) {
	requireIPv6Loopback(t)

	ipv4, err := net.Listen("tcp4", "0.0.0.0:0")
	require.NoError(t, err)
	defer ipv4.Close()
	port := ipv4.Addr().(*net.TCPAddr).Port

	ipv6, err := net.Listen("tcp6", net.JoinHostPort("::", strconv.Itoa(port)))
	require.NoError(t, err)
	defer ipv6.Close()

	set, err := bindMappingListeners(view.ExposurePublic, "", "tcp", port)
	require.Error(t, err)
	require.Nil(t, set)
}

func TestSpecificListenHostBindsOneFamily(t *testing.T) {
	set, err := bindMappingListeners(view.ExposurePublic, "127.0.0.1", "tcp", 0)
	require.NoError(t, err)
	require.NotNil(t, set)
	defer set.Close()

	require.Len(t, set.TCP, 1)
	require.Empty(t, set.Warnings(), "an explicit listen_host is not a partial dual-stack bind")
	require.Equal(t, netip.MustParseAddr("127.0.0.1"), listenerHost(t, set.TCP[0]))
	dialAndAccept(t, set.TCP[0], "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(set.Port())))
}

func TestUDPDualStack(t *testing.T) {
	requireIPv6Loopback(t)

	roundTrip := func(t *testing.T, set *mappingListenerSet, network, targetHost, clientIP string) {
		t.Helper()

		client, err := net.ListenUDP(network, &net.UDPAddr{IP: net.ParseIP(clientIP)})
		require.NoError(t, err)
		defer client.Close()

		target, err := net.ResolveUDPAddr(network, net.JoinHostPort(targetHost, strconv.Itoa(set.Port())))
		require.NoError(t, err)
		require.NoError(t, set.UDP.SetReadDeadline(time.Now().Add(2*time.Second)))
		_, err = client.WriteToUDP([]byte("ping-"+network), target)
		require.NoError(t, err)

		buf := make([]byte, 64)
		n, from, err := set.UDP.ReadFrom(buf)
		require.NoError(t, err)
		require.Equal(t, "ping-"+network, string(buf[:n]))
		require.NoError(t, set.UDP.SetReadDeadline(time.Time{}))

		udpFrom, ok := from.(*net.UDPAddr)
		require.True(t, ok, "unexpected peer address %T", from)
		_, err = set.UDP.WriteToUDPAddrPort([]byte("pong-"+network), udpFrom.AddrPort())
		require.NoError(t, err)

		require.NoError(t, client.SetReadDeadline(time.Now().Add(2*time.Second)))
		n, replyFrom, err := client.ReadFromUDP(buf)
		require.NoError(t, err)
		require.Equal(t, "pong-"+network, string(buf[:n]))
		if strings.HasSuffix(network, "4") {
			require.NotNil(t, replyFrom.IP.To4(), "IPv4 peer must receive the reply from the IPv4 socket")
		} else {
			require.Nil(t, replyFrom.IP.To4(), "IPv6 peer must receive the reply from the IPv6 socket")
		}
	}

	t.Run("local", func(t *testing.T) {
		set, err := bindMappingListeners(view.ExposureLocal, "192.0.2.10", "udp", 0)
		require.NoError(t, err)
		require.NotNil(t, set)
		require.Empty(t, set.Warnings())
		port := set.Port()

		roundTrip(t, set, "udp4", "127.0.0.1", "127.0.0.1")
		roundTrip(t, set, "udp6", "::1", "::1")

		require.NoError(t, set.Close())
		requireUDPPortReleased(t, "udp4", net.IPv4(127, 0, 0, 1), port)
		requireUDPPortReleased(t, "udp6", net.IPv6loopback, port)
	})

	t.Run("public", func(t *testing.T) {
		set, err := bindMappingListeners(view.ExposurePublic, "", "udp", 0)
		require.NoError(t, err)
		require.NotNil(t, set)
		require.Empty(t, set.Warnings())
		port := set.Port()

		roundTrip(t, set, "udp4", "127.0.0.1", "127.0.0.1")
		roundTrip(t, set, "udp6", "::1", "::1")

		require.NoError(t, set.Close())
		requireUDPPortReleased(t, "udp4", net.IPv4zero, port)
		requireUDPPortReleased(t, "udp6", net.IPv6unspecified, port)
	})
}

func TestMappingListenerSetCloseIsIdempotent(t *testing.T) {
	requireIPv6Loopback(t)

	set, err := bindMappingListeners(view.ExposureLocal, "", "tcp", 0)
	require.NoError(t, err)
	require.NoError(t, set.Close())
	require.NoError(t, set.Close())
}

func TestMappingListenerSetCloseJoinsUDPReaders(t *testing.T) {
	requireIPv6Loopback(t)

	set, err := bindMappingListeners(view.ExposureLocal, "", "udp", 0)
	require.NoError(t, err)

	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, _, err := set.UDP.ReadFrom(buf)
		readErr <- err
	}()

	require.NoError(t, set.Close())
	select {
	case err := <-readErr:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("blocked UDP read did not return after close")
	}
}

func TestBindMappingListenersRejectsUnknownType(t *testing.T) {
	set, err := bindMappingListeners(view.ExposurePublic, "", "sctp", 0)
	require.Error(t, err)
	require.Nil(t, set)
}
