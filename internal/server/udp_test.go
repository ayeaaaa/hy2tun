package server

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/testutil"
)

func TestReverseUDPEcho(t *testing.T) {
	echoAddr, stopEcho := testutil.StartUDPEcho(t)
	defer stopEcho()

	pair := startTestPair(t, []config.Mapping{
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1 && v[0].Mappings[0].Status == "running"
	}))
	port := pair.Server.Clients()[0].Mappings[0].AssignedPort

	conn, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 16)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := conn.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf[:n]))
}

// TestReverseUDPMultiplePeers proves that each public peer gets the reply that
// belongs to its own local socket. The tagged echo server prefixes every reply
// with the first byte of the request, so a shared client socket would deliver
// the wrong tag.
func TestReverseUDPMultiplePeers(t *testing.T) {
	echoAddr := startTaggedUDPEcho(t)

	pair := startTestPair(t, []config.Mapping{
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1 && v[0].Mappings[0].Status == "running"
	}))
	port := pair.Server.Clients()[0].Mappings[0].AssignedPort
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	type result struct {
		reply string
		err   error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i, payload := range []string{"a", "b"} {
		wg.Add(1)
		go func(i int, payload string) {
			defer wg.Done()
			conn, err := net.Dial("udp", addr)
			if err != nil {
				results[i].err = err
				return
			}
			defer conn.Close()
			if _, err := conn.Write([]byte(payload)); err != nil {
				results[i].err = err
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			buf := make([]byte, 64)
			n, err := conn.Read(buf)
			if err != nil {
				results[i].err = err
				return
			}
			results[i].reply = string(buf[:n])
		}(i, payload)
	}
	wg.Wait()

	for i, want := range []string{"aa", "bb"} {
		require.NoError(t, results[i].err, "peer %d", i)
		require.Equal(t, want, results[i].reply, "peer %d must receive its own reply", i)
	}
}

func TestReverseUDPMultipleMappings(t *testing.T) {
	echoAddr1 := startFixedTagUDPEcho(t, 'A')
	echoAddr2 := startFixedTagUDPEcho(t, 'B')

	pair := startTestPair(t, []config.Mapping{
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: echoAddr1, Enabled: true},
		{ID: "udp2", Type: "udp", RemotePort: 0, LocalAddr: echoAddr2, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 2 &&
			v[0].Mappings[0].Status == "running" && v[0].Mappings[1].Status == "running"
	}))
	views := pair.Server.Clients()
	port1 := views[0].Mappings[0].AssignedPort
	port2 := views[0].Mappings[1].AssignedPort
	require.NotEqual(t, port1, port2)

	got, err := exchangeUDP(fmt.Sprintf("127.0.0.1:%d", port1), []byte("one"), 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, "Aone", string(got))

	got, err = exchangeUDP(fmt.Sprintf("127.0.0.1:%d", port2), []byte("two"), 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, "Btwo", string(got))
}

// TestReverseUDPLargeDatagramDoesNotPanic sends datagrams at and above the HY2
// fragmentation threshold. A dropped datagram is acceptable; a panic or a
// permanently broken session is not.
func TestReverseUDPLargeDatagramDoesNotPanic(t *testing.T) {
	echoAddr, stopEcho := testutil.StartUDPEcho(t)
	defer stopEcho()

	pair := startTestPair(t, []config.Mapping{
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1 && v[0].Mappings[0].Status == "running"
	}))
	port := pair.Server.Clients()[0].Mappings[0].AssignedPort
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	for _, size := range []int{1200, 4096} {
		payload := bytes.Repeat([]byte{byte(size % 251)}, size)
		got, err := exchangeUDP(addr, payload, time.Second)
		if size == 1200 {
			require.NoError(t, err, "a 1200-byte datagram must be fragmented and echoed")
			require.Equal(t, payload, got)
			continue
		}
		// 4096 bytes do not fit in one HY2 UDPMessage once the header and
		// address are added, so a drop/timeout is the documented outcome.
		if err != nil {
			require.True(t, isTimeout(err), "unexpected error for %d-byte datagram: %v", size, err)
			continue
		}
		require.Equal(t, payload, got)
	}

	// The session must still work after the oversized attempts.
	payload := bytes.Repeat([]byte("ok"), 16)
	got, err := exchangeUDP(addr, payload, 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

func TestRemovingUDPMappingStopsPublicPort(t *testing.T) {
	echoAddr, stopEcho := testutil.StartUDPEcho(t)
	defer stopEcho()

	pair := startTestPair(t, []config.Mapping{
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1 && v[0].Mappings[0].Status == "running"
	}))
	views := pair.Server.Clients()
	clientID := views[0].ID
	port := views[0].Mappings[0].AssignedPort
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	got, err := exchangeUDP(addr, []byte("before"), 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, "before", string(got))

	require.NoError(t, pair.Server.removeMapping(clientID, "udp1"))
	require.Empty(t, pair.Server.Clients()[0].Mappings)

	conn, err := net.Dial("udp", addr)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("after"))
	require.NoError(t, err)
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, err = conn.Read(make([]byte, 16))
	require.Error(t, err, "a removed UDP mapping must stop answering on its public port")

	// The public port must be released.
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	require.NoError(t, err)
	require.NoError(t, udpConn.Close())
}

func TestDisablingUDPMappingStopsPublicPort(t *testing.T) {
	echoAddr, stopEcho := testutil.StartUDPEcho(t)
	defer stopEcho()

	pair := startTestPair(t, []config.Mapping{
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1 && v[0].Mappings[0].Status == "running"
	}))
	views := pair.Server.Clients()
	clientID := views[0].ID
	port := views[0].Mappings[0].AssignedPort
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	got, err := exchangeUDP(addr, []byte("before"), 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, "before", string(got))

	sess := testSession(t, pair.Server, clientID)
	revision := testSessionRevision(t, pair.Server, clientID)
	results, accepted, _ := pair.Server.mappings.update(sess, revision+1, []config.Mapping{
		{ID: "udp1", Type: "udp", RemotePort: port, LocalAddr: echoAddr, Enabled: false},
	})
	require.True(t, accepted)
	require.True(t, findResult(t, results, "udp1").OK)
	require.NoError(t, waitFor(t, 3*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1 && v[0].Mappings[0].Status == "disabled"
	}))

	conn, err := net.Dial("udp", addr)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("after"))
	require.NoError(t, err)
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, err = conn.Read(make([]byte, 16))
	require.Error(t, err, "a disabled UDP mapping must stop answering on its public port")

	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	require.NoError(t, err)
	require.NoError(t, udpConn.Close())
}

// TestPublicUDPConnDropsVirtualProbe exercises the wrapper directly: the
// virtual address is swallowed, while a real peer address is forwarded and
// reported by ReadFrom.
func TestPublicUDPConnDropsVirtualProbe(t *testing.T) {
	public, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer public.Close()

	peer, err := net.DialUDP("udp", nil, public.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer peer.Close()

	const virtualAddr = "_hy2tun_udp_probe:0"
	wrapper := &publicUDPConn{UDPConn: public, virtualAddr: virtualAddr}
	n, err := wrapper.WriteTo([]byte("probe"), virtualAddr)
	require.NoError(t, err)
	require.Equal(t, len("probe"), n)

	require.NoError(t, peer.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
	_, err = peer.Read(make([]byte, 8))
	require.Error(t, err)
	require.True(t, isTimeout(err), "virtual-address probe must be dropped, got %v", err)

	_, err = wrapper.WriteTo([]byte("reply"), peer.LocalAddr().String())
	require.NoError(t, err)
	buf := make([]byte, 16)
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, err = peer.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "reply", string(buf[:n]))

	_, err = peer.Write([]byte("incoming"))
	require.NoError(t, err)
	require.NoError(t, public.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, from, err := wrapper.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "incoming", string(buf[:n]))
	require.Equal(t, peer.LocalAddr().String(), from)

	require.NoError(t, wrapper.Close())
	_, err = wrapper.WriteTo([]byte("closed"), peer.LocalAddr().String())
	require.Error(t, err)
}

// TestPublicUDPConnCloseKeepsSharedSocketOpen simulates Hysteria closing one
// UDP session wrapper and verifies that the mapping's public socket stays open
// and can serve a new session.
func TestPublicUDPConnCloseKeepsSharedSocketOpen(t *testing.T) {
	public, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	defer public.Close()

	peer, err := net.DialUDP("udp", nil, public.LocalAddr().(*net.UDPAddr))
	require.NoError(t, err)
	defer peer.Close()

	const virtualAddr = "_hy2tun_udp_probe:0"
	first := &publicUDPConn{UDPConn: public, virtualAddr: virtualAddr, mappingID: "udp1"}

	readErr := make(chan error, 1)
	go func() {
		_, _, err := first.ReadFrom(make([]byte, 64))
		readErr <- err
	}()
	time.Sleep(50 * time.Millisecond)

	require.NoError(t, first.Close())
	select {
	case err := <-readErr:
		require.ErrorIs(t, err, net.ErrClosed)
	case <-time.After(time.Second):
		t.Fatal("closing a session wrapper did not unblock ReadFrom")
	}

	// A new session wrapper must be able to use the same public socket.
	second := &publicUDPConn{UDPConn: public, virtualAddr: virtualAddr, mappingID: "udp1"}
	defer second.Close()

	_, err = peer.Write([]byte("incoming"))
	require.NoError(t, err)
	buf := make([]byte, 64)
	n, from, err := second.ReadFrom(buf)
	require.NoError(t, err)
	require.Equal(t, "incoming", string(buf[:n]))
	require.Equal(t, peer.LocalAddr().String(), from)

	_, err = second.WriteTo([]byte("outgoing"), peer.LocalAddr().String())
	require.NoError(t, err)
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, err = peer.Read(buf)
	require.NoError(t, err)
	require.Equal(t, "outgoing", string(buf[:n]))

	// Closing the new wrapper must not close the mapping's socket either.
	require.NoError(t, second.Close())
	_, err = peer.Write([]byte("still-open"))
	require.NoError(t, err)
	require.NoError(t, public.SetReadDeadline(time.Now().Add(2*time.Second)))
	n, _, err = public.ReadFromUDP(buf)
	require.NoError(t, err)
	require.Equal(t, "still-open", string(buf[:n]))
}

// TestReverseUDPSessionCloseKeepsPublicPort closes the wrapper used by the
// live HY2 UDP session and verifies that the mapping's public socket remains
// open and can serve a replacement session.
func TestReverseUDPSessionCloseKeepsPublicPort(t *testing.T) {
	echoAddr, stopEcho := testutil.StartUDPEcho(t)
	defer stopEcho()

	pair := startTestPair(t, []config.Mapping{
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1 && v[0].Mappings[0].Status == "running"
	}))
	views := pair.Server.Clients()
	clientID := views[0].ID
	addr := fmt.Sprintf("127.0.0.1:%d", views[0].Mappings[0].AssignedPort)

	got, err := exchangeUDP(addr, []byte("before"), 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, "before", string(got))

	sess := testSession(t, pair.Server, clientID)
	pair.Server.mappings.mu.Lock()
	rt := sess.mappings["udp1"]
	pair.Server.mappings.mu.Unlock()
	require.NotNil(t, rt)

	rt.udpMu.Lock()
	session := rt.udpSession
	rt.udpMu.Unlock()
	require.NotNil(t, session, "the live HY2 session wrapper must be tracked")
	require.NoError(t, session.Close())

	// The public socket is still bound and a replacement wrapper can read a
	// datagram addressed to the public port.
	replacement := pair.Server.publicUDPConn(rt)
	require.NotNil(t, replacement)
	defer replacement.Close()

	peer, err := net.Dial("udp", addr)
	require.NoError(t, err)
	defer peer.Close()
	_, err = peer.Write([]byte("after-close"))
	require.NoError(t, err)

	type readResult struct {
		data []byte
		from string
		err  error
	}
	resultCh := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 64)
		n, from, err := replacement.ReadFrom(buf)
		resultCh <- readResult{data: append([]byte(nil), buf[:n]...), from: from, err: err}
	}()
	select {
	case result := <-resultCh:
		require.NoError(t, result.err)
		require.Equal(t, "after-close", string(result.data))
		require.NotEmpty(t, result.from)
	case <-time.After(2 * time.Second):
		t.Fatal("public socket did not receive a datagram after the HY2 session closed")
	}
}

// TestMappingRuntimeCloseClosesUDPPortOnce verifies that the mapping runtime,
// not a session wrapper, is the owner that releases the public UDP socket.
func TestMappingRuntimeCloseClosesUDPPortOnce(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	port := conn.LocalAddr().(*net.UDPAddr).Port
	rt := &mappingRuntime{id: "udp1", udpConn: conn, status: mappingStatusRunning}

	rt.close()
	rt.close()
	require.True(t, rt.closed.Load())

	rebound, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	require.NoError(t, err)
	require.NoError(t, rebound.Close())
}

func TestOutboundUDPRejectsUnknownVirtualToken(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	token := strings.Repeat("0", 32)
	o := &outbound{server: srv}
	_, err := o.UDP(udpVirtualAddress(token))
	require.Error(t, err)
	require.NotContains(t, err.Error(), token, "errors must not expose UDP tokens")
}

// startTaggedUDPEcho starts a UDP echo server that prefixes each reply with the
// first byte of the request.
func startTaggedUDPEcho(t *testing.T) string {
	t.Helper()
	return startUDPEchoServer(t, func(data []byte) byte { return data[0] })
}

// startFixedTagUDPEcho starts a UDP echo server that prefixes every reply with
// a fixed tag, identifying which target handled the request.
func startFixedTagUDPEcho(t *testing.T, tag byte) string {
	t.Helper()
	return startUDPEchoServer(t, func([]byte) byte { return tag })
}

func startUDPEchoServer(t *testing.T, tag func([]byte) byte) string {
	t.Helper()

	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	var wg sync.WaitGroup
	wg.Add(1)
	t.Cleanup(func() {
		_ = conn.Close()
		wg.Wait()
	})

	go func() {
		defer wg.Done()
		buf := make([]byte, 64*1024)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n == 0 {
				continue
			}
			reply := make([]byte, n+1)
			reply[0] = tag(buf[:n])
			copy(reply[1:], buf[:n])
			if _, err := conn.WriteToUDP(reply, from); err != nil {
				return
			}
		}
	}()
	return conn.LocalAddr().String()
}

// exchangeUDP sends one datagram to addr and waits up to wait for a reply.
func exchangeUDP(addr string, payload []byte, wait time.Duration) ([]byte, error) {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.Write(payload); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, len(payload)+1)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), buf[:n]...), nil
}
