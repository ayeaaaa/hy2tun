package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/ports"
	"hy2tun/internal/state"
	"hy2tun/internal/view"
)

// Mapping status values shown in the read models.
const (
	mappingStatusPending  = "pending"
	mappingStatusRunning  = "running"
	mappingStatusFailed   = "failed"
	mappingStatusDisabled = "disabled"
	mappingStatusOffline  = "offline"
)

// mappingRuntime is the server-side state of one registered mapping. Traffic
// counters are atomic because the data-plane goroutines added by later tasks
// update them concurrently with read models.
type mappingRuntime struct {
	// id is the immutable mapping identifier. Data-plane goroutines may read it
	// without the manager lock; mapping itself is mutable and must stay under
	// that lock.
	id string

	mapping      config.Mapping
	status       string
	errCode      string
	assignedPort int
	udpToken     string

	tcpListener net.Listener
	udpConn     *net.UDPConn
	data        *dataStreams
	closed      atomic.Bool
	closeOnce   sync.Once

	// udpMu serializes publicUDPConn creation with the eviction of the
	// previous HY2 UDP session wrapper. udpSession is the wrapper whose read
	// loop currently owns the public socket; only one session may read it.
	udpMu      sync.Mutex
	udpSession *publicUDPConn

	activeMu sync.Mutex
	active   map[net.Conn]struct{}

	txBytes atomic.Uint64
	rxBytes atomic.Uint64
}

// trackActive records a live data-plane connection for this mapping.
func (m *mappingRuntime) trackActive(conn net.Conn) {
	if conn == nil {
		return
	}
	m.activeMu.Lock()
	if m.active == nil {
		m.active = make(map[net.Conn]struct{})
	}
	m.active[conn] = struct{}{}
	m.activeMu.Unlock()
}

// untrackActive removes a connection from the active set.
func (m *mappingRuntime) untrackActive(conn net.Conn) {
	m.activeMu.Lock()
	delete(m.active, conn)
	m.activeMu.Unlock()
}

// closeActive closes every live connection for this mapping. It is used when a
// mapping is removed, disabled, or retargeted.
func (m *mappingRuntime) closeActive() {
	m.activeMu.Lock()
	conns := make([]net.Conn, 0, len(m.active))
	for conn := range m.active {
		conns = append(conns, conn)
	}
	m.active = make(map[net.Conn]struct{})
	m.activeMu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

// close releases the listener, UDP socket, pending data streams, and active
// connections owned by the runtime. The public UDP socket is owned by the
// runtime and is closed here exactly once.
func (m *mappingRuntime) close() {
	m.closeOnce.Do(func() {
		m.closed.Store(true)
		if m.tcpListener != nil {
			_ = m.tcpListener.Close()
		}
		if m.udpConn != nil {
			_ = m.udpConn.Close()
		}
		m.closeActive()
		if m.data != nil {
			m.data.dropMapping(m)
		}
	})
}

// clientSession is one live control stream and its registered mappings.
type clientSession struct {
	id       string
	name     string
	version  string
	revision int64
	online   bool
	lastSeen time.Time
	conn     net.Conn

	mappings map[string]*mappingRuntime
	order    []string

	// controlMu serializes configuration changes initiated by the client and
	// by the server's web API.
	controlMu sync.Mutex
	// writeGate serializes frames written to the control stream. It is a
	// channel rather than a mutex so a credential push can bound its wait
	// against the same absolute deadline as the write itself.
	writeGateOnce sync.Once
	writeGate     chan struct{}

	// pushWaitMu protects the single in-flight push waiter. PushAck frames
	// arrive on the control read loop while the push caller waits.
	pushWaitMu  sync.Mutex
	pushWait    chan *control.PushAck
	pushWaitID  string
	pushWaitRev int64

	// revision is the revision the client currently advertises. appliedRevision
	// is the revision of the mapping set this server has actually applied. A
	// push may only use a base where both agree; a conflict ack can advance
	// revision ahead of appliedRevision until the client's queued update lands.
	// Both are guarded by mappingManager.mu.
	appliedRevision int64

	// credentialMu guards the credential generations and sync flags. They are
	// written at Hello and may be refreshed after a credential push, while the
	// control loop and read models read them concurrently.
	credentialMu         sync.Mutex
	credentialGeneration int64
	// authenticatedGeneration is the generation whose control token
	// authenticated this stream. It is authoritative for revocation and
	// superseded-session cleanup; the reported generation is display-only.
	authenticatedGeneration int64
	credentialSynced        bool
	canCredentialUpdate     bool
	// credentialAck receives the next CredentialAck, including its OK/Error
	// result. The control read loop sends non-blockingly, so a late ack cannot
	// stall it.
	credentialAck chan *control.CredentialAck
	// credentialWaitMu guards credentialWaitGen. A rotation records the
	// generation it is pushing so the control loop can drop stale acks before
	// they occupy the single-slot ack channel.
	credentialWaitMu  sync.Mutex
	credentialWaitGen int64

	// ready is closed once the HelloAck has been written, so data-plane opens
	// cannot overtake the handshake. done is closed when the session is
	// detached.
	ready     chan struct{}
	done      chan struct{}
	readyOnce sync.Once
	doneOnce  sync.Once
}

// markReady allows data-plane opens to be sent on this session.
func (s *clientSession) markReady() {
	if s.ready == nil {
		return
	}
	s.readyOnce.Do(func() { close(s.ready) })
}

// markDone reports that the session is no longer usable.
func (s *clientSession) markDone() {
	if s.done == nil {
		return
	}
	s.doneOnce.Do(func() { close(s.done) })
}

// setCredentialState records the client's reported credential generation and
// flags. Both the reported and authenticated generations are set; callers that
// know the generation from the control token use setCredentialStateFor.
// Credential rotation uses it to refresh sessions after a push; readers use
// credentialState so the update is race-free.
func (s *clientSession) setCredentialState(generation int64, synced, capable bool) {
	s.setCredentialStateFor(generation, generation, synced, capable)
}

// setCredentialStateFor records the generation the client reported and the
// generation that actually authenticated its control token.
func (s *clientSession) setCredentialStateFor(reported, authenticated int64, synced, capable bool) {
	if s == nil {
		return
	}
	s.credentialMu.Lock()
	s.credentialGeneration = reported
	s.authenticatedGeneration = authenticated
	s.credentialSynced = synced
	s.canCredentialUpdate = capable
	s.credentialMu.Unlock()
}

// credentialStateSnapshot returns a consistent snapshot of both credential
// generations and the session flags.
func (s *clientSession) credentialStateSnapshot() (reported, authenticated int64, synced, capable bool) {
	if s == nil {
		return 0, 0, false, false
	}
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()
	return s.credentialGeneration, s.authenticatedGeneration, s.credentialSynced, s.canCredentialUpdate
}

// credentialState returns the client's reported generation and flags. The
// reported generation is for display; authentication and cleanup decisions use
// authenticatedCredentialState.
func (s *clientSession) credentialState() (generation int64, synced, capable bool) {
	generation, _, synced, capable = s.credentialStateSnapshot()
	return generation, synced, capable
}

// authenticatedCredentialState returns the generation whose control token
// authenticated the stream plus the session flags.
func (s *clientSession) authenticatedCredentialState() (generation int64, synced, capable bool) {
	_, generation, synced, capable = s.credentialStateSnapshot()
	return generation, synced, capable
}

// setCredentialWait records the generation a credential push is waiting for.
// Zero means no push is in flight.
func (s *clientSession) setCredentialWait(generation int64) {
	if s == nil {
		return
	}
	s.credentialWaitMu.Lock()
	s.credentialWaitGen = generation
	s.credentialWaitMu.Unlock()
}

// deliverCredentialAck forwards an ack to a waiting rotation. Acks for another
// generation are dropped so a stale ack cannot fill the buffered channel and
// hide the matching ack.
func (s *clientSession) deliverCredentialAck(ack *control.CredentialAck) bool {
	if s == nil || ack == nil {
		return false
	}
	s.credentialWaitMu.Lock()
	expected := s.credentialWaitGen
	s.credentialWaitMu.Unlock()
	if expected != 0 && ack.Generation != expected {
		return false
	}
	select {
	case s.credentialAck <- ack:
		return true
	default:
		return false
	}
}

// drainCredentialAcks removes acks left behind by an earlier timed-out push.
func (s *clientSession) drainCredentialAcks() {
	if s == nil {
		return
	}
	for {
		select {
		case <-s.credentialAck:
		default:
			return
		}
	}
}

// isReady reports whether the handshake response has been written. Credential
// pushes must not overtake the hello_ack.
func (s *clientSession) isReady() bool {
	if s == nil {
		return false
	}
	if s.ready == nil {
		return true
	}
	select {
	case <-s.ready:
		return true
	default:
		return false
	}
}

// waitReady blocks until the handshake response has been written, the session
// ends, or timeout elapses.
func (s *clientSession) waitReady(timeout time.Duration) error {
	if s.ready == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.ready:
		return nil
	case <-s.done:
		return fmt.Errorf("control session is closed")
	case <-timer.C:
		return fmt.Errorf("control session is not ready")
	}
}

// write sends one control frame with the default bounded write deadline.
func (s *clientSession) write(msg any) error {
	return s.writeWithDeadline(msg, time.Time{})
}

// lockWrite waits for the frame gate until deadline. The wait is bounded by
// the same absolute deadline as the write, so a credential push cannot consume
// its whole budget waiting behind another writer.
func (s *clientSession) lockWrite(deadline time.Time) bool {
	if s == nil {
		return false
	}
	s.writeGateOnce.Do(func() { s.writeGate = make(chan struct{}, 1) })
	if deadline.IsZero() {
		deadline = time.Now().Add(10 * time.Second)
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		return false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case s.writeGate <- struct{}{}:
		return true
	case <-timer.C:
		return false
	}
}

// unlockWrite releases the frame gate.
func (s *clientSession) unlockWrite() {
	if s == nil || s.writeGate == nil {
		return
	}
	<-s.writeGate
}

// writeWithDeadline sends one control frame with an absolute write deadline.
// A zero deadline means the default 10s budget. Credential pushes use the same
// deadline for lock acquisition, the write, and the ack wait so a client that
// stops reading cannot extend the rotation past its budget.
func (s *clientSession) writeWithDeadline(msg any, deadline time.Time) error {
	if s.conn == nil {
		return fmt.Errorf("control stream is closed")
	}
	if deadline.IsZero() {
		deadline = time.Now().Add(10 * time.Second)
	}
	if !s.lockWrite(deadline) {
		return context.DeadlineExceeded
	}
	defer s.unlockWrite()
	if !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}

	_ = s.conn.SetWriteDeadline(deadline)
	defer func() { _ = s.conn.SetWriteDeadline(time.Time{}) }()
	return control.WriteMessage(s.conn, msg)
}

// abortStream closes the control stream so the client reconnects and its hello
// re-registers the authoritative configuration. It is used when a push or
// update transaction can no longer trust the server's cached revision.
func (s *clientSession) abortStream() {
	if s == nil || s.conn == nil {
		return
	}
	_ = s.conn.Close()
}

// mappingManager owns all client sessions and serializes port allocation and
// binding across clients.
type mappingManager struct {
	mu sync.Mutex
	// policy is swapped atomically by hot configuration updates. Existing
	// mappings keep their listeners; only new registration and allocation read
	// the current policy.
	policy   atomic.Pointer[ports.Policy]
	store    *state.Store
	log      *slog.Logger
	bindHost string
	reserved map[int]struct{}
	data     *dataStreams

	sessions map[string]*clientSession

	// pendingUDP maps a UDP token from a pre-bound push runtime to that
	// runtime. udpRuntime consults it before the push commits, so the client's
	// probe cannot arrive before the runtime is visible. Entries are removed
	// on commit and rollback.
	pendingUDP map[string]*mappingRuntime

	// serveTCP handles public connections for a newly bound TCP mapping. It is
	// installed by Server.New before any mapping can be registered.
	serveTCP func(*clientSession, *mappingRuntime, net.Listener)
}

func newMappingManager(policy *ports.Policy, store *state.Store, log *slog.Logger, bindHost string, reserved map[int]struct{}, data *dataStreams) *mappingManager {
	if log == nil {
		log = slog.Default()
	}
	if reserved == nil {
		reserved = make(map[int]struct{})
	}
	m := &mappingManager{
		store:      store,
		log:        log,
		bindHost:   bindHost,
		reserved:   reserved,
		data:       data,
		sessions:   make(map[string]*clientSession),
		pendingUDP: make(map[string]*mappingRuntime),
	}
	m.policy.Store(policy)
	return m
}

// SetPolicy atomically installs a new allow/deny/auto policy. Mappings that are
// already bound are left running; later registrations and allocations use the
// new policy.
func (m *mappingManager) SetPolicy(policy *ports.Policy) {
	if m == nil || policy == nil {
		return
	}
	m.policy.Store(policy)
}

// newRuntime creates a mapping runtime attached to this manager's data-plane
// token registry.
func (m *mappingManager) newRuntime(mapping config.Mapping, status string) *mappingRuntime {
	return &mappingRuntime{
		id:      mapping.ID,
		mapping: mapping,
		status:  status,
		data:    m.data,
	}
}

// register replaces any previous session with the same client ID and applies
// the authoritative mapping list from Hello.
func (m *mappingManager) register(sess *clientSession, mappings []config.Mapping) []control.Result {
	now := time.Now()

	m.mu.Lock()
	old := m.sessions[sess.id]
	if old != nil {
		delete(m.sessions, sess.id)
		m.closeSessionLocked(old)
		if old.conn != nil {
			_ = old.conn.Close()
		}
	}

	sess.online = true
	sess.lastSeen = now
	m.sessions[sess.id] = sess
	results := m.applyLocked(sess, mappings)
	m.persistLocked(sess, now)
	m.mu.Unlock()

	m.saveState()
	return results
}

// update applies a new mapping list from the client. A nil list means an empty
// mapping list; callers that only change metadata use updateWithName.
func (m *mappingManager) update(sess *clientSession, revision int64, mappings []config.Mapping) ([]control.Result, bool, bool) {
	return m.updateWithName(sess, revision, mappings, "")
}

// updateWithName applies a mapping list and/or a display-name change. A nil
// mapping list is metadata-only only when name is non-empty; otherwise it is an
// explicit empty list, matching old clients that send `mappings:null` to clear
// their last mapping. It reports whether the update was accepted and whether
// the control stream must be closed so the client reconnects and re-registers
// its authoritative configuration. A rejected bind or malformed list requires
// a resync; a stale revision does not, because the client can simply learn the
// server's revision from the ack.
func (m *mappingManager) updateWithName(sess *clientSession, revision int64, mappings []config.Mapping, name string) ([]control.Result, bool, bool) {
	now := time.Now()

	m.mu.Lock()
	results, accepted, resync := m.updateLocked(sess, revision, mappings, name, now)
	m.mu.Unlock()

	if accepted {
		m.saveState()
	}
	return results, accepted, resync
}

// updateLocked diffs the new mapping list against the live session. Unchanged
// mappings keep their listener and UDP token; new or changed mappings are
// pre-bound while the old set is still running. Any pre-bind failure rolls back
// the new listeners and asks the caller to force a control-stream resync: the
// client's full configuration is authoritative, and its reconnect closes
// removed or disabled listeners that a wholesale rollback would leave running.
func (m *mappingManager) updateLocked(sess *clientSession, revision int64, mappings []config.Mapping, name string, now time.Time) ([]control.Result, bool, bool) {
	if m.sessions[sess.id] != sess {
		return nil, false, false
	}
	if revision <= sess.appliedRevision {
		return []control.Result{{Error: string(control.CodeStaleRevision)}}, false, false
	}
	if mappings == nil && name == "" {
		// Old clients send mappings:null when their mapping slice is empty.
		// Without a name there is no metadata to change, so this is an
		// explicit request to clear every mapping.
		mappings = []config.Mapping{}
	}
	if mappings == nil {
		// An absent mapping list with a name means the update only changes
		// metadata. Keep the live mapping set untouched; an explicit empty
		// slice still means "clear all mappings".
		sess.name = name
		sess.revision = revision
		sess.appliedRevision = revision
		sess.lastSeen = now
		m.persistLocked(sess, now)
		return m.currentResultsLocked(sess), true, false
	}

	results := make([]control.Result, len(mappings))
	next := make(map[string]config.Mapping, len(mappings))
	malformed := false
	for i, mapping := range mappings {
		results[i] = control.Result{ID: mapping.ID}
		switch {
		case mapping.ID == "":
			results[i].Error = string(control.CodeBadMessage)
			malformed = true
		default:
			if _, duplicate := next[mapping.ID]; duplicate {
				results[i].Error = string(control.CodeBadMessage)
				malformed = true
				continue
			}
			next[mapping.ID] = mapping
		}
	}
	if malformed {
		markRolledBack(results)
		return results, false, true
	}

	type updatePlan struct {
		mapping config.Mapping
		old     *mappingRuntime
		fresh   *mappingRuntime
		keep    bool
	}
	plans := make([]updatePlan, len(mappings))
	ignored := make(map[*mappingRuntime]struct{})
	for i, mapping := range mappings {
		old := sess.mappings[mapping.ID]
		plan := updatePlan{mapping: mapping, old: old}
		if old != nil {
			switch {
			case old.status == mappingStatusRunning:
				if mapping.Enabled &&
					old.mapping.Type == mapping.Type &&
					mappingPortUnchanged(old, mapping) {
					plan.keep = true
				}
			case old.status == mappingStatusDisabled:
				plan.keep = !mapping.Enabled
			}
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
	for i := range plans {
		plan := &plans[i]
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
	}
	if failed {
		for _, rt := range prebound {
			rt.close()
		}
		markRolledBack(results)
		return results, false, true
	}

	newMappings := make(map[string]*mappingRuntime, len(plans))
	newOrder := make([]string, 0, len(plans))
	for i := range plans {
		plan := &plans[i]
		if plan.keep {
			rt := plan.old
			localAddrChanged := rt.mapping.LocalAddr != plan.mapping.LocalAddr
			rt.mapping = plan.mapping
			if localAddrChanged {
				rt.closeActive()
			}
			newMappings[plan.mapping.ID] = rt
			newOrder = append(newOrder, plan.mapping.ID)
			continue
		}
		if plan.old != nil {
			plan.old.close()
		}
		newMappings[plan.mapping.ID] = plan.fresh
		newOrder = append(newOrder, plan.mapping.ID)
	}
	for id, old := range sess.mappings {
		if _, exists := next[id]; !exists {
			old.close()
		}
	}

	sess.mappings = newMappings
	sess.order = newOrder
	if name != "" {
		sess.name = name
	}
	sess.revision = revision
	sess.appliedRevision = revision
	sess.lastSeen = now
	m.persistLocked(sess, now)
	return results, true, false
}

// currentResultsLocked reports the live mapping set without changing it. It is
// used to acknowledge metadata-only updates that carry no mapping list.
func (m *mappingManager) currentResultsLocked(sess *clientSession) []control.Result {
	results := make([]control.Result, 0, len(sess.order))
	for _, id := range sess.order {
		rt := sess.mappings[id]
		if rt == nil {
			continue
		}
		results = append(results, control.Result{
			ID:           id,
			OK:           rt.errCode == "",
			AssignedPort: rt.assignedPort,
			UDPToken:     rt.udpToken,
			Error:        rt.errCode,
		})
	}
	return results
}

// mappingPortUnchanged reports whether next keeps the port currently bound for
// old. A requested port of 0 means "keep the server-assigned port".
func mappingPortUnchanged(old *mappingRuntime, next config.Mapping) bool {
	if next.RemotePort == 0 {
		return true
	}
	return next.RemotePort == old.assignedPort
}

// markRolledBack replaces successful results with an internal error so clients
// can tell that no part of a failed update was applied.
func markRolledBack(results []control.Result) {
	for i := range results {
		if results[i].Error == "" {
			results[i] = control.Result{ID: results[i].ID, Error: string(control.CodeInternal)}
		}
	}
}

// sessionRevision returns the current revision of sess, or zero when the
// session is no longer live.
func (m *mappingManager) sessionRevision(sess *clientSession) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[sess.id] != sess {
		return 0
	}
	return sess.revision
}

// detach removes sess when its control stream ends. A replacement session with
// the same ID is left untouched.
func (m *mappingManager) detach(sess *clientSession) {
	now := time.Now()

	m.mu.Lock()
	if m.sessions[sess.id] != sess {
		m.mu.Unlock()
		return
	}
	delete(m.sessions, sess.id)
	sess.online = false
	sess.lastSeen = now
	m.closeSessionLocked(sess)
	if m.store != nil {
		m.store.SetOnline(sess.id, false, now)
	}
	m.mu.Unlock()

	m.saveState()
}

// removeMapping removes one mapping from a live session, closing its listener
// and any traffic it is carrying. The change is persisted for offline
// reservations. It serializes with pushes and client updates through the
// per-client control queue.
func (m *mappingManager) removeMapping(clientID, mappingID string) error {
	m.mu.Lock()
	sess := m.sessions[clientID]
	m.mu.Unlock()
	if sess == nil {
		return control.ErrClientOffline
	}

	sess.controlMu.Lock()
	defer sess.controlMu.Unlock()

	now := time.Now()
	m.mu.Lock()
	if m.sessions[clientID] != sess {
		m.mu.Unlock()
		return control.ErrClientOffline
	}
	rt := sess.mappings[mappingID]
	if rt == nil {
		m.mu.Unlock()
		return fmt.Errorf("mapping %q not found", mappingID)
	}

	delete(sess.mappings, mappingID)
	for i, id := range sess.order {
		if id == mappingID {
			sess.order = append(sess.order[:i], sess.order[i+1:]...)
			break
		}
	}
	rt.close()
	m.persistLocked(sess, now)
	m.mu.Unlock()

	m.saveState()
	return nil
}

// closeAll closes every live session and marks it offline. It is used during
// server shutdown.
func (m *mappingManager) closeAll() {
	now := time.Now()

	m.mu.Lock()
	for id, sess := range m.sessions {
		delete(m.sessions, id)
		sess.online = false
		sess.lastSeen = now
		m.closeSessionLocked(sess)
		if sess.conn != nil {
			_ = sess.conn.Close()
		}
		if m.store != nil {
			m.store.SetOnline(id, false, now)
		}
	}
	m.mu.Unlock()

	m.saveState()
}

func (m *mappingManager) closeSessionLocked(sess *clientSession) {
	sess.markDone()
	m.closeMappingsLocked(sess)
}

func (m *mappingManager) closeMappingsLocked(sess *clientSession) {
	for _, rt := range sess.mappings {
		rt.close()
	}
}

// applyLocked binds every enabled mapping in order. A failure for one mapping
// does not stop the others.
func (m *mappingManager) applyLocked(sess *clientSession, mappings []config.Mapping) []control.Result {
	results := make([]control.Result, 0, len(mappings))
	sess.mappings = make(map[string]*mappingRuntime, len(mappings))
	sess.order = make([]string, 0, len(mappings))
	seen := make(map[string]struct{}, len(mappings))

	for _, mapping := range mappings {
		if mapping.ID == "" {
			results = append(results, control.Result{Error: string(control.CodeBadMessage)})
			continue
		}
		if _, duplicate := seen[mapping.ID]; duplicate {
			results = append(results, control.Result{ID: mapping.ID, Error: string(control.CodeBadMessage)})
			continue
		}
		seen[mapping.ID] = struct{}{}

		rt := m.newRuntime(mapping, mappingStatusPending)
		sess.mappings[mapping.ID] = rt
		sess.order = append(sess.order, mapping.ID)

		if !mapping.Enabled {
			rt.status = mappingStatusDisabled
			results = append(results, control.Result{ID: mapping.ID, OK: true})
			continue
		}
		results = append(results, m.bindLocked(sess, rt, nil, nil))
	}
	return results
}

// bindLocked allocates and binds one mapping while the manager lock is held,
// so no other client can claim the same port in between.
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

func (m *mappingManager) fail(rt *mappingRuntime, code control.ErrorCode) control.Result {
	rt.status = mappingStatusFailed
	rt.errCode = string(code)
	return control.Result{ID: rt.mapping.ID, Error: string(code)}
}

// acceptAndCloseTCP keeps a listener live but refuses traffic. It is only used
// when no data-plane handler has been installed.
func acceptAndCloseTCP(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

// portFreeLocked reports whether no live mapping of typ currently owns port.
// Runtimes in ignore are being replaced or removed by the current update and
// do not block their old ports; extra contains runtimes pre-bound by the same
// update and not yet committed.
func (m *mappingManager) portFreeLocked(typ string, port int, ignore map[*mappingRuntime]struct{}, extra []*mappingRuntime) bool {
	matches := func(rt *mappingRuntime) bool {
		return rt.status == mappingStatusRunning &&
			rt.mapping.Type == typ &&
			rt.assignedPort == port
	}
	for _, sess := range m.sessions {
		for _, rt := range sess.mappings {
			if ignore != nil {
				if _, skip := ignore[rt]; skip {
					continue
				}
			}
			if matches(rt) {
				return false
			}
		}
	}
	for _, rt := range extra {
		if matches(rt) {
			return false
		}
	}
	return true
}

// softReservedPortsLocked returns ports still reserved by other offline
// clients. The snapshot is taken once so auto-allocation does not repeatedly
// copy the whole state while scanning a range.
func (m *mappingManager) softReservedPortsLocked(clientID string) map[int]struct{} {
	if m.store == nil {
		return nil
	}
	now := time.Now()
	snapshot := m.store.Snapshot()
	reserved := make(map[int]struct{})
	for id, client := range snapshot.Clients {
		if id == clientID || client == nil || client.Online {
			continue
		}
		if !now.Before(client.ReservedUntil) {
			continue
		}
		for _, mapping := range client.Mappings {
			reserved[mapping.RemotePort] = struct{}{}
		}
	}
	return reserved
}

// persistLocked records the effective mapping list, with assigned ports filled
// in, so offline reservations survive a restart.
func (m *mappingManager) persistLocked(sess *clientSession, now time.Time) {
	if m.store == nil {
		return
	}
	mappings := make([]config.Mapping, 0, len(sess.order))
	for _, id := range sess.order {
		rt := sess.mappings[id]
		if rt == nil {
			continue
		}
		mapping := rt.mapping
		if rt.assignedPort > 0 {
			mapping.RemotePort = rt.assignedPort
		}
		mappings = append(mappings, mapping)
	}
	m.store.Upsert(state.ClientState{
		ID:       sess.id,
		Name:     sess.name,
		Version:  sess.version,
		Revision: sess.revision,
		Online:   true,
		LastSeen: now,
		Mappings: mappings,
	})
}

func (m *mappingManager) saveState() {
	if m.store == nil {
		return
	}
	if err := m.store.Save(); err != nil {
		m.log.Warn("save server state", "error", err)
	}
}

// overview aggregates live client, mapping, and traffic counters.
func (m *mappingManager) overview() (onlineClients, activeMappings int, txBytes, rxBytes uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, sess := range m.sessions {
		onlineClients++
		for _, rt := range sess.mappings {
			if rt.status == mappingStatusRunning {
				activeMappings++
			}
			txBytes += rt.txBytes.Load()
			rxBytes += rt.rxBytes.Load()
		}
	}
	return onlineClients, activeMappings, txBytes, rxBytes
}

// sessionEntry is a live session together with the metadata read under the
// manager lock. Credential pushes and status reads use the copy so a concurrent
// rename cannot race them.
type sessionEntry struct {
	sess *clientSession
	id   string
	name string
}

// sessionSnapshot returns the live sessions without holding the manager lock
// while callers send control messages or wait for acks.
func (m *mappingManager) sessionSnapshot() []sessionEntry {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sessionEntry, 0, len(m.sessions))
	for _, sess := range m.sessions {
		out = append(out, sessionEntry{sess: sess, id: sess.id, name: sess.name})
	}
	return out
}

// clientViews returns a consistent snapshot of live sessions merged with
// offline clients loaded from the state store.
func (m *mappingManager) clientViews() []view.ClientView {
	m.mu.Lock()
	defer m.mu.Unlock()

	online := make(map[string]struct{}, len(m.sessions))
	out := make([]view.ClientView, 0, len(m.sessions))
	for _, sess := range m.sessions {
		online[sess.id] = struct{}{}
		out = append(out, sessionView(sess))
	}

	if m.store != nil {
		snapshot := m.store.Snapshot()
		for id, client := range snapshot.Clients {
			if _, live := online[id]; live || client == nil {
				continue
			}
			clientView := view.ClientView{
				ID:       id,
				Name:     client.Name,
				Version:  client.Version,
				Revision: client.Revision,
				Online:   false,
				LastSeen: client.LastSeen,
			}
			for _, mapping := range client.Mappings {
				status := mappingStatusOffline
				if !mapping.Enabled {
					status = mappingStatusDisabled
				}
				clientView.Mappings = append(clientView.Mappings, view.MappingView{
					Mapping:      mapping,
					Status:       status,
					AssignedPort: mapping.RemotePort,
				})
			}
			out = append(out, clientView)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sessionView(sess *clientSession) view.ClientView {
	generation, synced, capable := sess.credentialState()
	clientView := view.ClientView{
		ID:                   sess.id,
		Name:                 sess.name,
		Version:              sess.version,
		Revision:             sess.revision,
		Online:               sess.online,
		LastSeen:             sess.lastSeen,
		CredentialGeneration: generation,
		CredentialSynced:     synced,
		CredentialCapable:    capable,
	}
	for _, id := range sess.order {
		rt := sess.mappings[id]
		if rt == nil {
			continue
		}
		clientView.Mappings = append(clientView.Mappings, view.MappingView{
			Mapping:      rt.mapping,
			Status:       rt.status,
			Error:        rt.errCode,
			AssignedPort: rt.assignedPort,
			TxBytes:      rt.txBytes.Load(),
			RxBytes:      rt.rxBytes.Load(),
		})
	}
	return clientView
}

func newUDPToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate UDP token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}

func addrPort(addr net.Addr) int {
	switch value := addr.(type) {
	case *net.TCPAddr:
		return value.Port
	case *net.UDPAddr:
		return value.Port
	default:
		_, portText, err := net.SplitHostPort(addr.String())
		if err != nil {
			return 0
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return 0
		}
		return port
	}
}
