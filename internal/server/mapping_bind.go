package server

import (
	"errors"
	"net"
	"strconv"

	"hy2tun/internal/control"
	"hy2tun/internal/ports"
)

func (m *mappingManager) bindLocked(sess *clientSession, rt *mappingRuntime, ignore map[*mappingRuntime]struct{}, extra []*mappingRuntime) control.Result {
	mapping := rt.mapping
	if mapping.Type != "tcp" && mapping.Type != "udp" {
		return m.fail(rt, control.CodeBadMessage)
	}
	if mapping.RemotePort < 0 || mapping.RemotePort > 65535 {
		return m.fail(rt, control.CodeBadMessage)
	}

	requested := mapping.RemotePort
	if requested > 0 {
		if _, reserved := m.reserved[requested]; reserved {
			return m.fail(rt, control.CodePortReserved)
		}
	}
	softReserved := m.softReservedPortsLocked(sess.id)
	policy := m.policy.Load()
	if policy == nil {
		return m.fail(rt, control.CodeInternal)
	}

	excluded := make(map[int]struct{})
	for {
		port, err := policy.Allocate(requested, func(candidate int) bool {
			if _, skip := excluded[candidate]; skip {
				return false
			}
			if _, reserved := m.reserved[candidate]; reserved {
				return false
			}
			return m.portFreeLocked(mapping.Type, candidate, ignore, extra)
		}, func(candidate int) bool {
			_, reserved := softReserved[candidate]
			return reserved
		})
		if err != nil {
			switch {
			case errors.Is(err, ports.ErrNotAllowed), errors.Is(err, ports.ErrDenied):
				return m.fail(rt, control.CodePortNotAllowed)
			case requested > 0:
				return m.fail(rt, control.CodePortInUse)
			default:
				return m.fail(rt, control.CodeNoFreePort)
			}
		}

		if mapping.Type == "tcp" {
			listener, err := net.Listen("tcp", net.JoinHostPort(m.bindHost, strconv.Itoa(port)))
			if err != nil {
				if requested > 0 {
					return m.fail(rt, control.CodePortInUse)
				}
				// An auto-allocated candidate may be held by a process
				// outside hy2tun; try the next one.
				excluded[port] = struct{}{}
				continue
			}
			rt.tcpListener = listener
			rt.assignedPort = addrPort(listener.Addr())
			rt.status = mappingStatusRunning
			if m.serveTCP != nil {
				go m.serveTCP(sess, rt, listener)
			} else {
				go acceptAndCloseTCP(listener)
			}
			return control.Result{ID: mapping.ID, OK: true, AssignedPort: rt.assignedPort}
		}

		udpAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(m.bindHost, strconv.Itoa(port)))
		if err != nil {
			return m.fail(rt, control.CodeInternal)
		}
		conn, err := net.ListenUDP("udp", udpAddr)
		if err != nil {
			if requested > 0 {
				return m.fail(rt, control.CodePortInUse)
			}
			excluded[port] = struct{}{}
			continue
		}
		token, err := newUDPToken()
		if err != nil {
			_ = conn.Close()
			return m.fail(rt, control.CodeInternal)
		}
		rt.udpConn = conn
		rt.assignedPort = addrPort(conn.LocalAddr())
		rt.udpToken = token
		rt.status = mappingStatusRunning
		return control.Result{
			ID:           mapping.ID,
			OK:           true,
			AssignedPort: rt.assignedPort,
			UDPToken:     rt.udpToken,
		}
	}
}
