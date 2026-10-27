package server

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/state"
	"hy2tun/internal/view"
)

// initState opens the persistent client state and reconciles clients that were
// recorded as online when the previous process exited. A client cannot be
// online before it reconnects, and treating it as offline makes its port
// reservations available to auto-allocation.
func (s *Server) initState() error {
	store, err := state.Open(s.cfg.StateFile, time.Duration(s.cfg.Ports.OfflineReserve))
	if err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	s.store = store

	now := time.Now()
	changed := false
	snapshot := store.Snapshot()
	for id, client := range snapshot.Clients {
		if client == nil || !client.Online {
			continue
		}
		store.SetOnline(id, false, now)
		changed = true
	}
	if changed {
		if err := store.Save(); err != nil {
			s.log.Warn("reconcile loaded client state", "error", err)
		}
	}
	return nil
}

// reservedPorts returns the server's own listening ports, which mappings may
// never claim even when the port policy would otherwise allow them.
func (s *Server) reservedPorts() map[int]struct{} {
	reserved := make(map[int]struct{})
	if port := addrPort(s.listenAddr); port > 0 {
		reserved[port] = struct{}{}
	}
	if _, portText, err := net.SplitHostPort(s.cfg.Web.Listen); err == nil {
		if port, err := strconv.Atoi(portText); err == nil && port > 0 {
			reserved[port] = struct{}{}
		}
	}
	return reserved
}

// Clients returns a snapshot of every known client, online and offline.
func (s *Server) Clients() []view.ClientView {
	return s.mappings.clientViews()
}

// Client returns one client view by ID.
func (s *Server) Client(id string) (view.ClientView, bool) {
	for _, client := range s.mappings.clientViews() {
		if client.ID == id {
			return client, true
		}
	}
	return view.ClientView{}, false
}

// Overview summarizes live clients and mappings.
func (s *Server) Overview() view.Overview {
	online, active, tx, rx := s.mappings.overview()
	return view.Overview{
		OnlineClients:  online,
		ActiveMappings: active,
		TxBytes:        tx,
		RxBytes:        rx,
		Listen:         s.listenAddr.String(),
		Uptime:         time.Since(s.startedAt),
	}
}

// PushMappings is implemented by the configuration-sync task. Until then every
// client is treated as unavailable for pushes.
func (s *Server) PushMappings(clientID string, baseRevision int64, mappings []config.Mapping) (int64, error) {
	return 0, control.ErrClientOffline
}
