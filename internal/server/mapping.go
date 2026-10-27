package server

import (
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
	mapping      config.Mapping
	status       string
	errCode      string
	assignedPort int
	udpToken     string

	tcpListener net.Listener
	udpConn     *net.UDPConn

	txBytes atomic.Uint64
	rxBytes atomic.Uint64
}

// close releases the listener or UDP socket owned by the runtime.
func (m *mappingRuntime) close() {
	if m.tcpListener != nil {
		_ = m.tcpListener.Close()
	}
	if m.udpConn != nil {
		_ = m.udpConn.Close()
	}
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
	// writeMu serializes frames written to the control stream.
	writeMu sync.Mutex
}

// write sends one control frame with a bounded write deadline.
func (s *clientSession) write(msg any) error {
	if s.conn == nil {
		return fmt.Errorf("control stream is closed")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = s.conn.SetWriteDeadline(time.Time{}) }()
	return control.WriteMessage(s.conn, msg)
}

// mappingManager owns all client sessions and serializes port allocation and
// binding across clients.
type mappingManager struct {
	mu       sync.Mutex
	policy   *ports.Policy
	store    *state.Store
	log      *slog.Logger
	bindHost string
	reserved map[int]struct{}

	sessions map[string]*clientSession
}

func newMappingManager(policy *ports.Policy, store *state.Store, log *slog.Logger, bindHost string, reserved map[int]struct{}) *mappingManager {
	if log == nil {
		log = slog.Default()
	}
	if reserved == nil {
		reserved = make(map[int]struct{})
	}
	return &mappingManager{
		policy:   policy,
		store:    store,
		log:      log,
		bindHost: bindHost,
		reserved: reserved,
		sessions: make(map[string]*clientSession),
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

// update applies a new mapping list from the client. It reports false when the
// revision is not newer than the one already recorded.
func (m *mappingManager) update(sess *clientSession, revision int64, mappings []config.Mapping) ([]control.Result, bool) {
	now := time.Now()

	m.mu.Lock()
	if m.sessions[sess.id] != sess {
		m.mu.Unlock()
		return nil, false
	}
	if revision <= sess.revision {
		m.mu.Unlock()
		return nil, false
	}

	m.closeMappingsLocked(sess)
	sess.revision = revision
	sess.lastSeen = now
	results := m.applyLocked(sess, mappings)
	m.persistLocked(sess, now)
	m.mu.Unlock()

	m.saveState()
	return results, true
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

		rt := &mappingRuntime{
			mapping: mapping,
			status:  mappingStatusPending,
		}
		sess.mappings[mapping.ID] = rt
		sess.order = append(sess.order, mapping.ID)

		if !mapping.Enabled {
			rt.status = mappingStatusDisabled
			results = append(results, control.Result{ID: mapping.ID, OK: true})
			continue
		}
		results = append(results, m.bindLocked(sess, rt))
	}
	return results
}

// bindLocked allocates and binds one mapping while the manager lock is held,
// so no other client can claim the same port in between.
func (m *mappingManager) bindLocked(sess *clientSession, rt *mappingRuntime) control.Result {
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

	excluded := make(map[int]struct{})
	for {
		port, err := m.policy.Allocate(requested, func(candidate int) bool {
			if _, skip := excluded[candidate]; skip {
				return false
			}
			if _, reserved := m.reserved[candidate]; reserved {
				return false
			}
			return m.portFreeLocked(mapping.Type, candidate)
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
			go m.acceptTCP(listener)
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

// acceptTCP accepts public TCP connections. Task 6 only proves the listener is
// live; the reverse data plane attaches in a later task.
func (m *mappingManager) acceptTCP(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = conn.Close()
	}
}

// portFreeLocked reports whether no live mapping of typ currently owns port.
func (m *mappingManager) portFreeLocked(typ string, port int) bool {
	for _, sess := range m.sessions {
		for _, rt := range sess.mappings {
			if rt.status != mappingStatusRunning {
				continue
			}
			if rt.mapping.Type == typ && rt.assignedPort == port {
				return false
			}
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
	clientView := view.ClientView{
		ID:       sess.id,
		Name:     sess.name,
		Version:  sess.version,
		Revision: sess.revision,
		Online:   sess.online,
		LastSeen: sess.lastSeen,
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
