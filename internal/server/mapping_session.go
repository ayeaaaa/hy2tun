package server

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"hy2tun/internal/control"
)

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
