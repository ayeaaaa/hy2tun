package server

import (
	"fmt"
	"net"
	"time"

	hyserver "github.com/apernet/hysteria/core/v2/server"
)

// outboundDialer bounds how long a proxied TCP connection may take to dial.
var outboundDialer = net.Dialer{Timeout: 10 * time.Second}

// outbound implements hyserver.Outbound. Normal addresses are dialed directly,
// giving standard Hysteria proxy behavior. The server field is used by later
// tasks to route reserved hy2tun virtual addresses.
type outbound struct {
	server *Server
}

// TCP dials reqAddr as a normal TCP destination. Reserved hy2tun virtual
// addresses are never dialed as real hosts.
func (o *outbound) TCP(reqAddr string) (net.Conn, error) {
	if kind, token, ok := parseVirtualAddr(reqAddr); ok {
		return o.virtualTCP(kind, token)
	}
	if isVirtualHost(reqAddr) {
		return nil, fmt.Errorf("reserved hy2tun address must not be dialed")
	}
	return outboundDialer.Dial("tcp", reqAddr)
}

// UDP opens a full-cone UDP socket. Hysteria sends all destinations of one
// client session through it. Reserved hy2tun virtual addresses are rejected.
func (o *outbound) UDP(reqAddr string) (hyserver.UDPConn, error) {
	if kind, token, ok := parseVirtualAddr(reqAddr); ok {
		return o.virtualUDP(kind, token)
	}
	if isVirtualHost(reqAddr) {
		return nil, fmt.Errorf("reserved hy2tun address must not be dialed")
	}
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}
	return &udpConn{UDPConn: conn}, nil
}

// CheckUDP permits every UDP destination. Access control is not part of this
// task.
func (o *outbound) CheckUDP(reqAddr string) error {
	return nil
}

// udpConn adapts *net.UDPConn to hyserver.UDPConn, which uses string addresses.
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
