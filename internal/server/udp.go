package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	hyserver "github.com/apernet/hysteria/core/v2/server"
	"hy2tun/internal/transport"
)

// udpVirtualAddress returns the reserved virtual address that carries one
// reverse UDP mapping. The token is per mapping and must never be logged.
func udpVirtualAddress(token string) string {
	return "_hy2tun_udp_" + token + ":0"
}

// publicUDPConn adapts a mapping's public UDP socket to hyserver.UDPConn.
// Hysteria creates one of these per HY2 UDP session, but the underlying socket
// is owned by the mapping runtime and shared by every session that maps to it.
// Closing a wrapper only detaches that session; it must never close the socket.
type publicUDPConn struct {
	UDPConn     udpPacketConn
	virtualAddr string
	mappingID   string
	runtime     *mappingRuntime
	log         *slog.Logger

	mu       sync.Mutex
	closed   bool
	readDone chan struct{}
	bridge   transport.DatagramConn
}

// ReadFrom reads one public datagram and reports the public peer address, which
// Hysteria puts into UDPMessage.Addr for the client.
//
// A closing sibling session may set a read deadline on the shared socket to
// wake its own blocked read. That deadline is cleared here and the read is
// retried, so it cannot terminate the current session.
func (c *publicUDPConn) ReadFrom(b []byte) (int, string, error) {
	done := make(chan struct{})
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		close(done)
		return 0, "", net.ErrClosed
	}
	c.readDone = done
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		if c.readDone == done {
			c.readDone = nil
		}
		close(done)
		c.mu.Unlock()
	}()

	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return 0, "", net.ErrClosed
		}
		_ = c.UDPConn.SetReadDeadline(time.Time{})
		c.mu.Unlock()

		n, addr, err := c.UDPConn.ReadFrom(b)
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return 0, "", net.ErrClosed
		}
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				// Another session wrapper was closed and poked the shared
				// deadline. Clear it and keep reading.
				continue
			}
			return n, "", err
		}
		if n == len(b) {
			c.logger().Warn("public UDP datagram may be truncated",
				"mapping", c.mappingID,
				"size", n,
				"buffer", len(b),
			)
		}
		if n > 0 && c.runtime != nil {
			c.runtime.rxBytes.Add(uint64(n))
		}
		if addr == nil {
			return n, "", nil
		}
		return n, addr.String(), nil
	}
}

// WriteTo sends a datagram to a public peer. The mapping's virtual address
// carries only the probe and keepalive, so it is dropped after refreshing the
// HY2 UDP session.
func (c *publicUDPConn) WriteTo(b []byte, addr string) (int, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	if addr == c.virtualAddr {
		return len(b), nil
	}
	// Reverse-tunnel metadata is the public peer's numeric IP/port, not a
	// hostname target. Parse IPv4/IPv6 (including zones) without DNS and without
	// holding wrapper/manager locks across an address lookup or socket write.
	udpAddr, err := netip.ParseAddrPort(addr)
	if err != nil {
		return 0, err
	}
	n, err := c.UDPConn.WriteToUDPAddrPort(b, udpAddr)
	if n > 0 && c.runtime != nil {
		c.runtime.txBytes.Add(uint64(n))
	}
	return n, err
}

// Close detaches this HY2 UDP session from the mapping's public socket. The
// mapping runtime owns the socket and closes it on removal/disable; closing a
// wrapper must not take the public port down. The read deadline wakes a blocked
// ReadFrom so the session's goroutine can exit.
func (c *publicUDPConn) Close() error {
	c.mu.Lock()
	if c.closed {
		// Another goroutine already detached this session. Wait for its
		// blocked read to drain so a replacement wrapper cannot race it.
		done := c.readDone
		c.mu.Unlock()
		if done != nil {
			<-done
		}
		return nil
	}
	c.closed = true
	done := c.readDone
	c.mu.Unlock()

	err := c.UDPConn.SetReadDeadline(time.Now())
	if c.bridge != nil {
		_ = c.bridge.Close()
	}
	if done != nil {
		<-done
	}
	return err
}

func (c *publicUDPConn) logger() *slog.Logger {
	if c.log != nil {
		return c.log
	}
	return slog.Default()
}

// publicUDPConn wraps m's public UDP socket for one reverse UDP session. If a
// previous session wrapper is still attached, it is detached and its blocked
// read is drained before the new wrapper starts reading. This keeps exactly one
// session consuming datagrams from the shared public socket.
func (s *Server) publicUDPConn(m *mappingRuntime) *publicUDPConn {
	return s.attachPublicUDPConn(m, nil)
}

func (s *Server) attachPublicUDPConn(m *mappingRuntime, bridge transport.DatagramConn) *publicUDPConn {
	if m == nil {
		return nil
	}

	// Lock before reading the listener set: an exposure switch replaces it
	// while holding the same mutex, so a racing attachment cannot capture the
	// sockets that are being closed.
	m.udpMu.Lock()
	defer m.udpMu.Unlock()
	if m.closed.Load() || m.listeners == nil || m.listeners.UDP == nil {
		return nil
	}
	c := &publicUDPConn{
		UDPConn:     m.listeners.UDP,
		virtualAddr: udpVirtualAddress(m.udpToken),
		mappingID:   m.id,
		runtime:     m,
		log:         s.log,
		bridge:      bridge,
	}

	old := m.udpSession
	if old != nil {
		_ = old.Close()
	}
	if m.closed.Load() {
		return nil
	}
	m.udpSession = c
	// Clear the wake deadline set by the old session before the new session
	// starts reading.
	_ = m.listeners.UDP.SetReadDeadline(time.Time{})
	return c
}

func (s *Server) bridgeTCPUDP(rt *mappingRuntime, public *publicUDPConn, datagrams transport.DatagramConn) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer datagrams.Close()
		// One extra byte detects over-limit public packets instead of relaying
		// truncated payloads. The adapter owns the bounded 32-packet send queue.
		buf := make([]byte, 4097)
		for {
			n, addr, err := public.ReadFrom(buf)
			if err != nil {
				return
			}
			if n > 4096 || len(addr) > 256 {
				rt.udpDrops.Add(1)
				continue
			}
			if err := datagrams.Send(buf[:n], addr); err != nil {
				if errors.Is(err, transport.ErrQueueFull) {
					rt.udpDrops.Add(1)
					continue
				}
				return
			}
		}
	}()
	for {
		payload, addr, err := datagrams.Receive()
		if err != nil {
			break
		}
		if _, err := public.WriteTo(payload, addr); err != nil {
			break
		}
	}
	_ = public.Close()
	_ = datagrams.Close()
	<-done
	rt.udpMu.Lock()
	if rt.udpSession == public {
		rt.udpSession = nil
	}
	rt.udpMu.Unlock()
}

// UDP tokens are reusable, but only by the exact live TCP runtime owner. The
// legacy HY2 path above remains a bearer-token route (no authenticated caller).
func (m *mappingManager) udpRuntimeFor(token string, owner *clientSession) (*mappingRuntime, error) {
	if m == nil || token == "" {
		return nil, fmt.Errorf("hy2tun UDP mapping is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.liveTCPOwnerLocked(owner) {
		return nil, fmt.Errorf("hy2tun UDP owner is unavailable")
	}
	valid := func(rt *mappingRuntime) bool {
		return rt != nil && rt.owner == owner && rt.udpToken == token && rt.mapping.Type == "udp" && rt.status == mappingStatusRunning && !rt.closed.Load()
	}
	if rt := m.pendingUDP[token]; valid(rt) {
		return rt, nil
	}
	for _, rt := range owner.mappings {
		if valid(rt) {
			return rt, nil
		}
	}
	return nil, fmt.Errorf("hy2tun UDP mapping is unavailable")
}

// udpRuntime returns the running UDP mapping registered under token. The token
// itself is never included in an error or log message. Pre-bound push runtimes
// are visible before the push commits so a client probe sent immediately after
// its push_ack is not lost.
func (m *mappingManager) udpRuntime(token string) (*mappingRuntime, error) {
	if m == nil || token == "" {
		return nil, fmt.Errorf("hy2tun UDP mapping is not available")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if rt := m.pendingUDP[token]; rt != nil &&
		rt.mapping.Type == "udp" &&
		rt.status == mappingStatusRunning &&
		!rt.closed.Load() {
		return rt, nil
	}
	for _, sess := range m.sessions {
		for _, rt := range sess.mappings {
			if rt.udpToken == token &&
				rt.mapping.Type == "udp" &&
				rt.status == mappingStatusRunning &&
				!rt.closed.Load() {
				return rt, nil
			}
		}
	}
	return nil, fmt.Errorf("hy2tun UDP mapping is not registered")
}

// registerPendingUDPLocked publishes a pre-bound UDP runtime to udpRuntime
// before the push transaction commits. The caller must hold m.mu.
func (m *mappingManager) registerPendingUDPLocked(rt *mappingRuntime) {
	if rt == nil || rt.udpToken == "" {
		return
	}
	if m.pendingUDP == nil {
		m.pendingUDP = make(map[string]*mappingRuntime)
	}
	m.pendingUDP[rt.udpToken] = rt
}

// dropPendingUDPLocked removes rt from the pending token map. The caller must
// hold m.mu.
func (m *mappingManager) dropPendingUDPLocked(rt *mappingRuntime) {
	if rt == nil || rt.udpToken == "" {
		return
	}
	if m.pendingUDP[rt.udpToken] == rt {
		delete(m.pendingUDP, rt.udpToken)
	}
}

// udpConn adapts *net.UDPConn to hyserver.UDPConn for normal proxy traffic.
type udpConn struct {
	*net.UDPConn
}

func (c *udpConn) ReadFrom(b []byte) (int, string, error) {
	n, addr, err := c.UDPConn.ReadFrom(b)
	if addr == nil {
		return n, "", err
	}
	return n, addr.String(), err
}

func (c *udpConn) WriteTo(b []byte, addr string) (int, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return 0, err
	}
	return c.UDPConn.WriteTo(b, udpAddr)
}

var _ hyserver.UDPConn = (*publicUDPConn)(nil)
