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
	for _, portRange := range s.cfg.ListenRanges {
		for port := portRange.Start; port <= portRange.End; port++ {
			if port > 0 {
				reserved[port] = struct{}{}
			}
		}
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

// DeleteClient removes an offline client and its mappings/reservations.
func (s *Server) DeleteClient(id string) error {
	return s.mappings.deleteClient(id)
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

// pushAckTimeout bounds how long the server waits for a client to acknowledge
// a mapping push. It is a variable so tests can exercise timeout recovery
// without waiting ten seconds.
var pushAckTimeout = 10 * time.Second

// pushReadyTimeout bounds how long a push waits for a freshly published
// session to finish its hello/hello_ack handshake.
const pushReadyTimeout = 5 * time.Second

// PushError reports a push rejected before the client changed anything. It
// carries one result per requested mapping so a caller can show which port or
// address failed.
type PushError struct {
	Results []control.Result
}

// Error implements error.
func (e *PushError) Error() string {
	if e == nil {
		return "mapping push rejected"
	}
	for _, result := range e.Results {
		if result.Error == "" {
			continue
		}
		if result.ID != "" {
			return fmt.Sprintf("mapping push rejected: mapping %s: %s", result.ID, result.Error)
		}
		return fmt.Sprintf("mapping push rejected: %s", result.Error)
	}
	return "mapping push rejected"
}

// MappingResults returns the per-mapping outcomes. It lets callers that cannot
// import this package, such as the server web UI, inspect the failure through a
// small local interface.
func (e *PushError) MappingResults() []control.Result {
	if e == nil {
		return nil
	}
	return e.Results
}

// PushMappings applies an authoritative mapping list to an online client. It
// pre-binds new or changed ports, sends a push, and commits only after the
// client acknowledges it. baseRevision must equal the client's current
// revision; a mismatch returns control.ErrConflict.
func (s *Server) PushMappings(clientID string, baseRevision int64, mappings []config.Mapping) (int64, error) {
	if s == nil || s.mappings == nil {
		return 0, control.ErrClientOffline
	}
	return s.mappings.pushMappings(clientID, baseRevision, mappings)
}

// pushPlan is the transaction state of one mapping in a push.
type pushPlan struct {
	mapping config.Mapping
	old     *mappingRuntime
	fresh   *mappingRuntime
	keep    bool
}

// pushMappings implements the push transaction.
func (m *mappingManager) pushMappings(clientID string, baseRevision int64, mappings []config.Mapping) (int64, error) {
	m.mu.Lock()
	sess := m.sessions[clientID]
	if sess == nil || !sess.online {
		m.mu.Unlock()
		return 0, control.ErrClientOffline
	}
	m.mu.Unlock()

	// The session is published before hello_ack is written. A web push must
	// not overtake that handshake, so wait for the session to become ready.
	// A session that ends while waiting is offline; a timeout is retryable.
	if err := sess.waitReady(pushReadyTimeout); err != nil {
		select {
		case <-sess.done:
			return 0, control.ErrClientOffline
		default:
			return 0, control.ErrClientNotReady
		}
	}

	// Serialize this push with client updates and other pushes for the same
	// session, then re-check that the session is still the live one.
	sess.controlMu.Lock()
	defer sess.controlMu.Unlock()

	m.mu.Lock()
	if m.sessions[clientID] != sess {
		m.mu.Unlock()
		return 0, control.ErrClientOffline
	}
	// The client's advertised revision and the revision of the mapping set the
	// server actually applied must agree. After a conflict ack the client may
	// advertise a newer revision while its queued update has not been applied;
	// a retry must wait for that update instead of pushing over it.
	if baseRevision != sess.revision || baseRevision != sess.appliedRevision {
		m.mu.Unlock()
		return 0, control.ErrConflict
	}
	plans, results, err := m.preparePushLocked(sess, mappings)
	if err != nil {
		m.mu.Unlock()
		return 0, err
	}
	effective := effectivePushMappings(plans)
	m.mu.Unlock()

	rollback := func() {
		m.mu.Lock()
		for _, plan := range plans {
			if plan.fresh != nil {
				m.dropPendingUDPLocked(plan.fresh)
			}
		}
		m.mu.Unlock()
		for _, plan := range plans {
			if plan.fresh != nil {
				plan.fresh.close()
			}
		}
	}

	pushID, err := newPushID()
	if err != nil {
		rollback()
		return 0, fmt.Errorf("generate push ID: %w", err)
	}
	revision := baseRevision + 1
	ackCh := make(chan *control.PushAck, 1)
	sess.setPushWaiter(ackCh, pushID, revision)
	defer sess.clearPushWaiter(ackCh)

	if err := sess.write(&control.Push{
		Type:         control.TypePush,
		ID:           pushID,
		BaseRevision: baseRevision,
		Revision:     revision,
		Mappings:     effective,
		Results:      results,
	}); err != nil {
		rollback()
		// The client may still be running its old mapping set, but the stream
		// is unusable. Close it so the reconnect re-registers the
		// authoritative configuration instead of leaving a revision split.
		m.log.Warn("closing control stream after push write failure",
			"client", sess.id,
			"error", err,
		)
		sess.abortStream()
		return 0, fmt.Errorf("send mapping push: %w", err)
	}

	timer := time.NewTimer(pushAckTimeout)
	defer timer.Stop()

	for {
		select {
		case ack := <-ackCh:
			if !pushAckMatches(ack, pushID, revision) {
				// A late ack from an earlier timed-out push must not poison
				// this transaction. Keep waiting for the matching ack.
				continue
			}
			return m.finishPush(sess, plans, revision, ack, rollback)
		case <-sess.done:
			rollback()
			return 0, control.ErrClientOffline
		case <-timer.C:
			rollback()
			// The client may have applied the push and written its config
			// before the ack was lost. Closing the stream forces a hello that
			// makes the client's file authoritative again.
			m.log.Warn("closing control stream after push timeout", "client", sess.id)
			sess.abortStream()
			return 0, control.ErrPushTimeout
		}
	}
}

// pushAckMatches reports whether ack belongs to the push identified by pushID
// and revision. Every ack must echo the push ID, so a late ack from an earlier
// push cannot be mistaken for this one. Successful acks must also echo the push
// revision. Failure acks carry the client's revision instead, so they are
// accepted once their ID matches.
func pushAckMatches(ack *control.PushAck, pushID string, revision int64) bool {
	if ack == nil || pushID == "" || ack.ID != pushID {
		return false
	}
	if ack.OK {
		return ack.Revision == revision
	}
	return true
}

// newPushID returns a 128-bit random identifier that correlates a push with
// its acknowledgement.
func newPushID() (string, error) {
	return newDataToken()
}

// preparePushLocked validates the requested list and pre-binds only new or
// retargeted mappings while the old set is still serving traffic. Any failure
// closes the new listeners and leaves the session untouched.
func (m *mappingManager) preparePushLocked(sess *clientSession, mappings []config.Mapping) ([]*pushPlan, []control.Result, error) {
	results := make([]control.Result, len(mappings))
	next := make(map[string]config.Mapping, len(mappings))
	malformed := false
	for i, mapping := range mappings {
		results[i] = control.Result{ID: mapping.ID}
		if err := validatePushMapping(mapping); err != nil {
			results[i].Error = string(control.CodeBadMessage)
			malformed = true
			continue
		}
		if _, duplicate := next[mapping.ID]; duplicate {
			results[i].Error = string(control.CodeBadMessage)
			malformed = true
			continue
		}
		next[mapping.ID] = mapping
	}
	if malformed {
		markRolledBack(results)
		return nil, results, &PushError{Results: results}
	}

	plans := make([]*pushPlan, len(mappings))
	ignored := make(map[*mappingRuntime]struct{})
	for i, mapping := range mappings {
		old := sess.mappings[mapping.ID]
		plan := &pushPlan{mapping: mapping, old: old}
		switch {
		case old == nil:
		case mapping.Enabled &&
			old.status == mappingStatusRunning &&
			old.mapping.Type == mapping.Type &&
			mappingPortUnchanged(old, mapping):
			plan.keep = true
		case !mapping.Enabled && old.status == mappingStatusDisabled:
			plan.keep = true
		}
		if !plan.keep && old != nil {
			ignored[old] = struct{}{}
		}
		plans[i] = plan
	}
	for id, old := range sess.mappings {
		if _, exists := next[id]; !exists {
			ignored[old] = struct{}{}
		}
	}

	var prebound []*mappingRuntime
	failed := false
	for i, plan := range plans {
		if plan.keep {
			results[i] = control.Result{
				ID:           plan.mapping.ID,
				OK:           true,
				AssignedPort: plan.old.assignedPort,
				UDPToken:     plan.old.udpToken,
			}
			continue
		}
		if !plan.mapping.Enabled {
			plan.fresh = m.newRuntime(plan.mapping, mappingStatusDisabled)
			results[i] = control.Result{ID: plan.mapping.ID, OK: true}
			continue
		}

		rt := m.newRuntime(plan.mapping, mappingStatusPending)
		result := m.bindLocked(sess, rt, ignored, prebound)
		results[i] = result
		if !result.OK {
			failed = true
			continue
		}
		plan.fresh = rt
		prebound = append(prebound, rt)
		m.registerPendingUDPLocked(rt)
	}
	if failed {
		for _, rt := range prebound {
			m.dropPendingUDPLocked(rt)
			rt.close()
		}
		markRolledBack(results)
		return nil, results, &PushError{Results: results}
	}
	return plans, results, nil
}

// effectivePushMappings fills in assigned ports so the client persists the
// actual public port for every remote_port: 0 mapping.
func effectivePushMappings(plans []*pushPlan) []config.Mapping {
	effective := make([]config.Mapping, 0, len(plans))
	for _, plan := range plans {
		mapping := plan.mapping
		switch {
		case plan.keep && plan.old != nil:
			if mapping.RemotePort == 0 && plan.old.assignedPort > 0 {
				mapping.RemotePort = plan.old.assignedPort
			}
		case plan.fresh != nil:
			if mapping.RemotePort == 0 && plan.fresh.assignedPort > 0 {
				mapping.RemotePort = plan.fresh.assignedPort
			}
		}
		effective = append(effective, mapping)
	}
	return effective
}

// finishPush handles the client's ack and commits or rolls back the prepared
// runtimes.
func (m *mappingManager) finishPush(sess *clientSession, plans []*pushPlan, revision int64, ack *control.PushAck, rollback func()) (int64, error) {
	if ack == nil {
		rollback()
		return 0, fmt.Errorf("mapping push: empty push ack")
	}
	if !ack.OK {
		rollback()
		if ack.Error == string(control.CodeConflict) {
			m.adoptClientRevision(sess, ack.Revision)
			return 0, control.ErrConflict
		}
		return 0, fmt.Errorf("mapping push rejected by client: %s", ack.Error)
	}
	if ack.Revision != revision {
		rollback()
		// An OK ack for a different revision means the client applied
		// something the server did not prepare. Reconnect so hello re-syncs
		// the authoritative configuration.
		m.log.Warn("closing control stream after push ack mismatch",
			"client", sess.id,
			"ack_revision", ack.Revision,
			"push_revision", revision,
		)
		sess.abortStream()
		return 0, fmt.Errorf("mapping push: client acknowledged revision %d, want %d", ack.Revision, revision)
	}

	now := time.Now()
	m.mu.Lock()
	if m.sessions[sess.id] != sess {
		m.mu.Unlock()
		rollback()
		return 0, control.ErrClientOffline
	}
	m.commitPushLocked(sess, plans, revision, now)
	m.mu.Unlock()

	m.saveState()
	return revision, nil
}

// commitPushLocked installs the pre-bound runtimes, closes removed or replaced
// ones, and persists the new mapping list and revision.
func (m *mappingManager) commitPushLocked(sess *clientSession, plans []*pushPlan, revision int64, now time.Time) {
	next := make(map[string]*mappingRuntime, len(plans))
	order := make([]string, 0, len(plans))
	for _, plan := range plans {
		if plan.keep {
			rt := plan.old
			localAddrChanged := rt.mapping.LocalAddr != plan.mapping.LocalAddr
			rt.mapping = plan.mapping
			if localAddrChanged {
				rt.closeActive()
			}
			next[plan.mapping.ID] = rt
			order = append(order, plan.mapping.ID)
			continue
		}
		if plan.old != nil {
			plan.old.close()
		}
		if plan.fresh != nil {
			m.dropPendingUDPLocked(plan.fresh)
			next[plan.mapping.ID] = plan.fresh
			order = append(order, plan.mapping.ID)
		}
	}
	for id, old := range sess.mappings {
		if _, exists := next[id]; !exists {
			old.close()
		}
	}

	sess.mappings = next
	sess.order = order
	sess.revision = revision
	sess.appliedRevision = revision
	sess.lastSeen = now
	m.persistLocked(sess, now)
}

// adoptClientRevision records the revision reported by a conflicting push ack.
// appliedRevision is deliberately left unchanged: the server still has the old
// mapping set until the client's queued update arrives, and a push retry must
// not use the adopted revision as its base.
func (m *mappingManager) adoptClientRevision(sess *clientSession, revision int64) {
	if revision < 0 {
		return
	}
	now := time.Now()

	m.mu.Lock()
	if m.sessions[sess.id] == sess {
		sess.revision = revision
		sess.lastSeen = now
		m.persistLocked(sess, now)
	}
	m.mu.Unlock()

	m.saveState()
}

// validatePushMapping rejects mappings that could never be bound.
func validatePushMapping(mapping config.Mapping) error {
	if mapping.ID == "" {
		return fmt.Errorf("mapping ID is required")
	}
	if mapping.Type != "tcp" && mapping.Type != "udp" {
		return fmt.Errorf("mapping %q has an invalid type", mapping.ID)
	}
	if mapping.RemotePort < 0 || mapping.RemotePort > 65535 {
		return fmt.Errorf("mapping %q has an invalid remote port", mapping.ID)
	}
	host, portText, err := net.SplitHostPort(mapping.LocalAddr)
	if err != nil {
		return fmt.Errorf("mapping %q has an invalid local_addr", mapping.ID)
	}
	if host == "" {
		return fmt.Errorf("mapping %q local_addr must include a host", mapping.ID)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("mapping %q has an invalid local_addr port", mapping.ID)
	}
	return nil
}
