package server

import (
	"errors"

	"hy2tun/internal/control"
	"hy2tun/internal/ports"
)

func (m *mappingManager) bindLocked(sess *clientSession, rt *mappingRuntime, ignore map[*mappingRuntime]struct{}, extra []*mappingRuntime) control.Result {
	rt.owner = sess
	mapping := rt.mapping
	if mapping.Type != "tcp" && mapping.Type != "udp" {
		return m.fail(rt, control.CodeBadMessage)
	}
	if mapping.RemotePort < 0 || mapping.RemotePort > 65535 {
		return m.fail(rt, control.CodeBadMessage)
	}
	// A corrupt or hand-edited state file must not silently reopen a mapping
	// publicly. Empty is the legacy public default; any other invalid value
	// fails closed.
	if rt.exposure != "" && !rt.exposure.Valid() {
		m.log.Warn("mapping has an invalid stored exposure; refusing to bind",
			"mapping", rt.id,
			"exposure", rt.exposure,
		)
		return m.fail(rt, control.CodeInternal)
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

		listeners, err := bindMappingListeners(rt.exposure, m.bindHost, mapping.Type, port)
		if err != nil {
			if requested > 0 {
				return m.fail(rt, control.CodePortInUse)
			}
			// An auto-allocated candidate may be held by a process outside
			// hy2tun; try the next one.
			excluded[port] = struct{}{}
			continue
		}

		for _, family := range listeners.Warnings() {
			m.log.Warn("mapping address family unavailable",
				"mapping", rt.id,
				"family", family,
				"error", listeners.familyErrors[family],
			)
		}

		rt.listeners = listeners
		rt.assignedPort = listeners.Port()
		rt.errCode = ""
		if mapping.Type == "tcp" {
			rt.connSem = make(chan struct{}, maxConnectionsPerMapping)
			rt.status = mappingStatusRunning
			m.startTCPListenersLocked(sess, rt, listeners)
			return control.Result{
				ID:                  mapping.ID,
				OK:                  true,
				AssignedPort:        rt.assignedPort,
				UnavailableFamilies: listeners.Warnings(),
				Exposure:            string(exposureOrDefault(rt.exposure)),
			}
		}

		token, err := newUDPToken()
		if err != nil {
			_ = listeners.Close()
			return m.fail(rt, control.CodeInternal)
		}
		rt.udpToken = token
		rt.status = mappingStatusRunning
		return control.Result{
			ID:                  mapping.ID,
			OK:                  true,
			AssignedPort:        rt.assignedPort,
			UDPToken:            rt.udpToken,
			UnavailableFamilies: listeners.Warnings(),
			Exposure:            string(exposureOrDefault(rt.exposure)),
		}
	}
}
