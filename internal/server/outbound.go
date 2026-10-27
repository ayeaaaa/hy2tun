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
// giving standard Hysteria proxy behavior. Reserved hy2tun virtual addresses
// are routed to the control, data, or UDP mapping plane.
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
// client session through it. Reserved hy2tun UDP addresses are routed to the
// owning mapping's public socket; other virtual addresses are rejected.
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
