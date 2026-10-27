package server

import (
	"fmt"
	"net"
	"strings"

	hyserver "github.com/apernet/hysteria/core/v2/server"
)

const (
	// virtualHostPrefix marks hosts reserved by hy2tun. Such names must never
	// be resolved or dialed as real hosts.
	virtualHostPrefix = "_hy2tun_"

	virtualKindControl = "control"
	virtualKindData    = "data"
	virtualKindUDP     = "udp"
)

// parseVirtualAddr reports whether addr is a reserved hy2tun virtual address
// and, if so, returns its kind and token. The port is ignored. The recognized
// forms are:
//
//	_hy2tun_control_:0
//	_hy2tun_data_<token>:0
//	_hy2tun_udp_<token>:0
func parseVirtualAddr(addr string) (kind, token string, ok bool) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", false
	}
	rest, found := strings.CutPrefix(host, virtualHostPrefix)
	if !found {
		return "", "", false
	}
	switch {
	case rest == virtualKindControl+"_":
		return virtualKindControl, "", true
	case strings.HasPrefix(rest, virtualKindData+"_"):
		if token := strings.TrimPrefix(rest, virtualKindData+"_"); token != "" {
			return virtualKindData, token, true
		}
	case strings.HasPrefix(rest, virtualKindUDP+"_"):
		if token := strings.TrimPrefix(rest, virtualKindUDP+"_"); token != "" {
			return virtualKindUDP, token, true
		}
	}
	return "", "", false
}

// isVirtualHost reports whether addr uses the reserved hy2tun host prefix, even
// if it is not one of the recognized virtual address forms.
func isVirtualHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return strings.HasPrefix(host, virtualHostPrefix)
}

// virtualTCP handles a reserved virtual address. Routing for the control and
// reverse data planes is added by later tasks; until then every virtual
// address is rejected instead of being dialed as a real host.
func (o *outbound) virtualTCP(kind, token string) (net.Conn, error) {
	return nil, fmt.Errorf("hy2tun virtual %q address is not supported", kind)
}

// virtualUDP handles a reserved virtual address on the UDP path. Reverse UDP
// routing is added by a later task.
func (o *outbound) virtualUDP(kind, token string) (hyserver.UDPConn, error) {
	return nil, fmt.Errorf("hy2tun virtual %q address is not supported", kind)
}
