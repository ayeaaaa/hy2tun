package config

import (
	"fmt"
	"net"
	"strings"
)

// TransportMode selects the tunnel transports permitted by a configuration.
// The empty value preserves legacy configs and behaves as ModeHY2 at runtime.
type TransportMode string

const (
	ModeHY2  TransportMode = "hy2"
	ModeTCP  TransportMode = "tcp"
	ModeAuto TransportMode = "hy2+tcp"
)

// Effective returns the runtime mode without materializing a legacy default.
func (m TransportMode) Effective() TransportMode {
	if m == "" {
		return ModeHY2
	}
	return m
}

// Validate rejects unsupported transport modes.
func (m TransportMode) Validate() error {
	switch m.Effective() {
	case ModeHY2, ModeTCP, ModeAuto:
		return nil
	default:
		return fmt.Errorf("mode must be %q, %q, or %q", ModeHY2, ModeTCP, ModeAuto)
	}
}

// AllowsHY2 reports whether HY2 is permitted by the effective mode.
func (m TransportMode) AllowsHY2() bool {
	mode := m.Effective()
	return mode == ModeHY2 || mode == ModeAuto
}

// AllowsTCP reports whether TCP is permitted by the effective mode.
func (m TransportMode) AllowsTCP() bool {
	mode := m.Effective()
	return mode == ModeTCP || mode == ModeAuto
}

// validateTCPPort rejects port-set syntax even when it denotes one unique port.
// Callers also validate the address using their client/server-specific parser.
func validateTCPPort(value string) error {
	_, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return err
	}
	if strings.ContainsAny(port, ",-") {
		return fmt.Errorf("mode tcp requires a single port without ranges or port hopping")
	}
	return nil
}
