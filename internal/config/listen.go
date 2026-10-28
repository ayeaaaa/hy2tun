package config

import (
	"fmt"
	"net"
	"strings"

	"hy2tun/internal/ports"
)

// ParseListen parses a server listen address as host:port or host:start-end.
// Comma-separated ranges are accepted for compatibility with the client's
// port-set syntax, but the first range's start is the actual bind port.
func ParseListen(value string) (string, []ports.Range, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil, fmt.Errorf("listen is required")
	}
	host, portStr, err := net.SplitHostPort(value)
	if err != nil {
		return "", nil, fmt.Errorf("listen must be a valid host:port or port range: %w", err)
	}
	if strings.TrimSpace(portStr) == "0" {
		return host, []ports.Range{{Start: 0, End: 0}}, nil
	}
	ranges, err := ports.ParseRanges(strings.Split(portStr, ","))
	if err != nil {
		return "", nil, fmt.Errorf("listen: %w", err)
	}
	if len(ranges) == 0 {
		return "", nil, fmt.Errorf("listen must include at least one port")
	}
	return host, ranges, nil
}
