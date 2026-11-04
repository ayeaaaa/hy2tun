package server

import (
	"fmt"

	"hy2tun/internal/config"
	"hy2tun/internal/transport"
)

// Mode returns the effective transport mode currently persisted and served.
func (s *Server) Mode() config.TransportMode {
	cfg := s.currentConfig()
	if cfg == nil {
		return ""
	}
	return cfg.Mode.Effective()
}

// UpdateMode hot-applies a transport mode. New endpoints are bound before the
// configuration is persisted, and are served only after a successful write.
// Existing endpoints, sessions, mappings, the state store and the Web console
// are left untouched unless their own transport is being added or retired.
func (s *Server) UpdateMode(mode config.TransportMode) error {
	if s == nil {
		return fmt.Errorf("update mode: server is nil")
	}
	if err := mode.Validate(); err != nil {
		return fmt.Errorf("update mode: %w", err)
	}

	// Serialize listener transactions with Close and other mode updates. No
	// settings or listener lock is held while binding a candidate.
	s.modeMu.Lock()
	defer s.modeMu.Unlock()

	if s.closed.Load() {
		return fmt.Errorf("update mode: server is closed")
	}
	cfg := s.currentConfig()
	if cfg == nil {
		return fmt.Errorf("update mode: server config is nil")
	}
	desired := mode.Effective()

	s.listenersMu.Lock()
	existing := make(map[transport.Kind]endpoint, len(s.endpoints))
	for kind, ep := range s.endpoints {
		existing[kind] = ep
	}
	s.listenersMu.Unlock()

	candidates := make(map[transport.Kind]endpoint)
	closeCandidates := func() {
		for _, ep := range candidates {
			_ = ep.Close()
		}
	}
	for _, kind := range []transport.Kind{transport.HY2, transport.TCP} {
		wanted := (kind == transport.HY2 && desired.AllowsHY2()) || (kind == transport.TCP && desired.AllowsTCP())
		if !wanted || existing[kind] != nil {
			continue
		}
		ep, err := s.prepareEndpoint(kind, cfg, s.baseBindAddr)
		if err != nil {
			closeCandidates()
			return fmt.Errorf("update mode: %w", err)
		}
		candidates[kind] = ep
	}

	// Merge the mode into the latest configuration so a concurrent credential
	// rotation cannot be overwritten by the snapshot used for binding.
	s.settingsMu.Lock()
	if s.closed.Load() {
		s.settingsMu.Unlock()
		closeCandidates()
		return fmt.Errorf("update mode: server is closed")
	}
	latest := cloneConfig(s.cfg)
	if latest == nil {
		s.settingsMu.Unlock()
		closeCandidates()
		return fmt.Errorf("update mode: server config is nil")
	}
	latest.Mode = mode
	if err := latest.Validate(); err != nil {
		s.settingsMu.Unlock()
		closeCandidates()
		return fmt.Errorf("update mode: %w", err)
	}
	if err := s.persistConfig(latest); err != nil {
		s.settingsMu.Unlock()
		closeCandidates()
		return fmt.Errorf("update mode: %w", err)
	}
	s.cfg = latest
	s.settingsMu.Unlock()

	// Publish the new endpoint set atomically. Removal also detaches only that
	// transport's sessions; shared state and the Web console are not closed.
	var added []endpoint
	var removed []endpoint
	s.listenersMu.Lock()
	if s.closed.Load() {
		s.listenersMu.Unlock()
		closeCandidates()
		return fmt.Errorf("update mode: server is closed")
	}
	for kind, ep := range candidates {
		s.endpoints[kind] = ep
		if s.mappings != nil {
			// reserved is read under the manager lock by bindLocked; keep the
			// established listenersMu -> manager.mu order.
			s.mappings.mu.Lock()
			s.mappings.reserved[addrPort(ep.Addr())] = struct{}{}
			s.mappings.mu.Unlock()
		}
		if hy, ok := ep.(*hy2Endpoint); ok {
			s.hyServer, s.packetConn = hy.server, hy.packetConn
		}
		added = append(added, ep)
	}
	for kind, ep := range s.endpoints {
		if (kind == transport.HY2 && !desired.AllowsHY2()) || (kind == transport.TCP && !desired.AllowsTCP()) {
			delete(s.endpoints, kind)
			delete(s.serving, ep)
			ep.markRetired()
			if s.mappings != nil {
				s.mappings.detachTransport(kind)
			}
			if kind == transport.HY2 {
				s.hyServer, s.packetConn = nil, nil
			}
			removed = append(removed, ep)
		}
	}
	if s.started.Load() {
		for _, ep := range added {
			s.startEndpointLocked(ep)
		}
	}
	s.listenersMu.Unlock()

	for _, ep := range removed {
		if err := ep.Close(); err != nil {
			s.log.Warn("close retired endpoint", "transport", ep.Kind(), "error", err)
		}
	}
	return nil
}
