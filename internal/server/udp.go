package server

import (
	"fmt"
	"net"

	hyserver "github.com/apernet/hysteria/core/v2/server"
)

// udpVirtualAddress returns the reserved virtual address that carries one
// reverse UDP mapping. The token is per mapping and must never be logged.
func udpVirtualAddress(token string) string {
	return "_hy2tun_udp_" + token + ":0"
}

// publicUDPConn adapts a mapping's public UDP socket to hyserver.UDPConn.
// Hysteria creates one of these per HY2 UDP session; the underlying socket is
// owned by the mapping runtime and shared by every session that maps to it.
type publicUDPConn struct {
	*net.UDPConn
	virtualAddr string
}

// ReadFrom reads one public datagram and reports the public peer address, which
// Hysteria puts into UDPMessage.Addr for the client.
func (c *publicUDPConn) ReadFrom(b []byte) (int, string, error) {
	n, addr, err := c.UDPConn.ReadFrom(b)
	if addr == nil {
		return n, "", err
	}
	return n, addr.String(), err
}

// WriteTo sends a datagram to a public peer. The mapping's virtual address
// carries only the probe and keepalive, so it is dropped after refreshing the
// HY2 UDP session.
func (c *publicUDPConn) WriteTo(b []byte, addr string) (int, error) {
	if addr == c.virtualAddr {
		return len(b), nil
	}
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return 0, err
	}
	return c.UDPConn.WriteTo(b, udpAddr)
}

// Close releases the public socket. The mapping runtime also closes it during
// teardown, so a second close returns net.ErrClosed, which Hysteria ignores.
func (c *publicUDPConn) Close() error {
	return c.UDPConn.Close()
}

// publicUDPConn wraps m's public UDP socket for one reverse UDP session.
func (s *Server) publicUDPConn(m *mappingRuntime) *publicUDPConn {
	if m == nil || m.udpConn == nil {
		return nil
	}
	return &publicUDPConn{
		UDPConn:     m.udpConn,
		virtualAddr: udpVirtualAddress(m.udpToken),
	}
}

// udpRuntime returns the running UDP mapping registered under token. The token
// itself is never included in an error or log message.
func (m *mappingManager) udpRuntime(token string) (*mappingRuntime, error) {
	if m == nil || token == "" {
		return nil, fmt.Errorf("hy2tun UDP mapping is not available")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
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
