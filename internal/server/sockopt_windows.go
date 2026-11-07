//go:build windows

package server

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// setIPv6Only prevents an IPv6 wildcard socket from also claiming the IPv4
// wildcard port, so the two family sockets can coexist on one assigned port.
func setIPv6Only(_, _ string, raw syscall.RawConn) error {
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = windows.SetsockoptInt(
			windows.Handle(fd),
			windows.IPPROTO_IPV6,
			windows.IPV6_V6ONLY,
			1,
		)
	}); err != nil {
		return err
	}
	return sockErr
}
