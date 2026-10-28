//go:build !linux

package firewall

import (
	"errors"
	"io"
	"net"

	"hy2tun/internal/ports"
)

func SetupUDPPortRedirect(*net.UDPAddr, []ports.Range) (io.Closer, error) {
	return nil, errors.New("port hopping server ranges are only supported on Linux")
}
