package server

import (
	"errors"
	"fmt"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/state"
	"hy2tun/internal/view"
)

var (
	// ErrMappingNotFound reports that a known client has no mapping with the
	// requested ID.
	ErrMappingNotFound = errors.New("mapping not found")
	// ErrExposureInvalid reports a requested exposure other than public/local.
	ErrExposureInvalid = errors.New("invalid mapping exposure")
)

// MappingNotFoundError identifies the client and mapping that could not be
// found. MappingNotFound lets the Web API classify the error without importing
// this package (the server imports the Web UI, so the reverse import would
// cycle).
type MappingNotFoundError struct {
	ClientID  string
	MappingID string
}

func (e *MappingNotFoundError) Error() string {
	if e == nil {
		return ErrMappingNotFound.Error()
	}
	if e.MappingID == "" {
		return fmt.Sprintf("%v: client %q", ErrMappingNotFound, e.ClientID)
	}
	return fmt.Sprintf("%v: client %q mapping %q", ErrMappingNotFound, e.ClientID, e.MappingID)
}

// Unwrap supports errors.Is(err, ErrMappingNotFound).
func (e *MappingNotFoundError) Unwrap() error { return ErrMappingNotFound }

// MappingNotFound marks the error class for cross-package classification.
func (e *MappingNotFoundError) MappingNotFound() bool { return true }

// ExposureInvalidError reports a value outside the public/local set.
// InvalidExposure lets the Web API classify the error without importing this
// package.
type ExposureInvalidError struct {
	Value view.Exposure
}

func (e *ExposureInvalidError) Error() string {
	if e == nil {
		return ErrExposureInvalid.Error()
	}
	return fmt.Sprintf("%v: %q", ErrExposureInvalid, e.Value)
}

// Unwrap supports errors.Is(err, ErrExposureInvalid).
func (e *ExposureInvalidError) Unwrap() error { return ErrExposureInvalid }

// InvalidExposure marks the error class for cross-package classification.
func (e *ExposureInvalidError) InvalidExposure() bool { return true }

// ExposureSwitchError reports a live exposure switch that could not be applied.
// Exposure is the exposure still active after rollback (or the previous
// exposure when the mapping is left closed), and Closed reports that no
// listener remains. Persistence records whether the original failure was a
// state save rather than a bind/rollback error; both are reported as 409 with
// the retained active state because the API must never hide what remains
// exposed. It never contains secret material.
type ExposureSwitchError struct {
	Requested           view.Exposure
	Exposure            view.Exposure
	UnavailableFamilies []string
	Closed              bool
	Persistence         bool
	Err                 error
}

func (e *ExposureSwitchError) Error() string {
	if e == nil {
		return "mapping exposure switch failed"
	}
	requested := exposureOrDefault(e.Requested)
	stateText := fmt.Sprintf("mapping remains %s", exposureOrDefault(e.Exposure))
	if e.Closed {
		stateText = "mapping is closed"
	}
	if e.Err != nil {
		return fmt.Sprintf("switch mapping exposure to %s: %s: %v", requested, stateText, e.Err)
	}
	return fmt.Sprintf("switch mapping exposure to %s: %s", requested, stateText)
}

// Unwrap returns the bind or persistence failure that caused the rollback.
func (e *ExposureSwitchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ExposureConflict marks a failed switch for cross-package classification.
// Persistence failures also carry the active state that remains in effect, so
// they are conflicts too; only errors without a known exposure/listener state
// fall through to 500.
func (e *ExposureSwitchError) ExposureConflict() bool {
	return e != nil
}

// ActiveExposure returns the exposure that remains active after rollback.
func (e *ExposureSwitchError) ActiveExposure() view.Exposure {
	if e == nil {
		return ""
	}
	return exposureOrDefault(e.Exposure)
}

// ActiveUnavailableFamilies returns a copy of the families that stayed
// unavailable after rollback.
func (e *ExposureSwitchError) ActiveUnavailableFamilies() []string {
	if e == nil {
		return nil
	}
	return append([]string(nil), e.UnavailableFamilies...)
}

// MappingClosed reports whether the rollback also failed and left the mapping
// without listeners.
func (e *ExposureSwitchError) MappingClosed() bool {
	return e != nil && e.Closed
}

// ExposurePersistError reports a desired-exposure save failure for a mapping
// that has no live listener to switch (offline client, disabled mapping, or
// already-closed runtime). Exposure is the previously stored value that
// remains in effect, and Closed reports that no listener is active. It is a
// conflict for the Web API because the caller must be able to report what
// stayed in effect instead of a generic 500.
type ExposurePersistError struct {
	Requested           view.Exposure
	Exposure            view.Exposure
	UnavailableFamilies []string
	Closed              bool
	Err                 error
}

func (e *ExposurePersistError) Error() string {
	if e == nil {
		return "persist mapping exposure failed"
	}
	requested := exposureOrDefault(e.Requested)
	stateText := fmt.Sprintf("mapping remains %s", exposureOrDefault(e.Exposure))
	if e.Closed {
		stateText = "mapping has no listener"
	}
	if e.Err != nil {
		return fmt.Sprintf("persist mapping exposure %s: %s: %v", requested, stateText, e.Err)
	}
	return fmt.Sprintf("persist mapping exposure %s: %s", requested, stateText)
}

// Unwrap returns the persistence failure.
func (e *ExposurePersistError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ExposureConflict marks the failure for cross-package 409 classification.
func (e *ExposurePersistError) ExposureConflict() bool { return e != nil }

// ActiveExposure returns the exposure that remains in effect.
func (e *ExposurePersistError) ActiveExposure() view.Exposure {
	if e == nil {
		return ""
	}
	return exposureOrDefault(e.Exposure)
}

// ActiveUnavailableFamilies returns a copy of the families that remain
// unavailable.
func (e *ExposurePersistError) ActiveUnavailableFamilies() []string {
	if e == nil {
		return nil
	}
	return append([]string(nil), e.UnavailableFamilies...)
}

// MappingClosed reports whether the mapping currently has no listener.
func (e *ExposurePersistError) MappingClosed() bool {
	return e != nil && e.Closed
}

// exposureOrDefault maps the legacy empty value to public and leaves any
// invalid stored value visible so it cannot be silently rewritten.
func exposureOrDefault(exposure view.Exposure) view.Exposure {
	if exposure == "" {
		return view.ExposurePublic
	}
	return exposure
}

// exposureFor returns the stored exposure for one mapping. Empty is the legacy
// public default. Invalid non-empty values are returned unchanged so the bind
// path fails closed instead of silently exposing the mapping.
func (m *mappingManager) exposureFor(clientID, mappingID string) view.Exposure {
	if m == nil || m.store == nil {
		return view.ExposurePublic
	}
	return exposureOrDefault(view.Exposure(m.store.Exposure(clientID, mappingID)))
}

// mappingWarnings returns the families a live runtime could not bind. It is
// used by control results and read models so both report the same list.
func mappingWarnings(rt *mappingRuntime) []string {
	if rt == nil || rt.listeners == nil {
		return nil
	}
	return rt.listeners.Warnings()
}

// mappingViewForRuntime builds the read model for a live runtime.
func mappingViewForRuntime(rt *mappingRuntime) view.MappingView {
	if rt == nil {
		return view.MappingView{}
	}
	return view.MappingView{
		Mapping:             rt.mapping,
		Status:              rt.status,
		Error:               rt.errCode,
		AssignedPort:        rt.assignedPort,
		Exposure:            exposureOrDefault(rt.exposure),
		UnavailableFamilies: mappingWarnings(rt),
		TxBytes:             rt.txBytes.Load(),
		RxBytes:             rt.rxBytes.Load(),
	}
}

// SetMappingExposure is the server-level entry point used by the Web API and
// CLI. It returns the updated read model on success.
func (s *Server) SetMappingExposure(clientID, mappingID string, exposure view.Exposure) (view.MappingView, error) {
	if s == nil || s.mappings == nil {
		return view.MappingView{}, control.ErrClientNotFound
	}
	return s.mappings.SetExposure(clientID, mappingID, exposure)
}

// SetExposure validates the requested value and applies it to a live mapping or
// stores it as the desired state for the next bind. A live running mapping is
// serialized with client updates and pushes through controlMu; the old
// listeners and connections are closed, the new set is bound, the exposure is
// persisted, and only then does the mapping start accepting again. A total bind
// or persistence failure restores the previous listeners and exposure.
func (m *mappingManager) SetExposure(clientID, mappingID string, exposure view.Exposure) (view.MappingView, error) {
	if !exposure.Valid() {
		return view.MappingView{}, &ExposureInvalidError{Value: exposure}
	}
	if mappingID == "" {
		return view.MappingView{}, &MappingNotFoundError{ClientID: clientID}
	}
	if m == nil {
		return view.MappingView{}, control.ErrClientNotFound
	}
	// A session can be replaced between looking it up and locking its control
	// queue. Retry a few times, then report the churn instead of silently
	// updating only half of the state.
	for attempt := 0; attempt < 4; attempt++ {
		m.mu.Lock()
		sess := m.sessions[clientID]
		m.mu.Unlock()

		if sess == nil {
			// Offline state-only path: exposureMu excludes registration while
			// m.mu protects the snapshot/persist transaction. No controlMu is
			// involved because there is no session.
			m.exposureMu.Lock()
			m.mu.Lock()
			if m.sessions[clientID] != nil {
				// A session appeared while waiting; retry on the live path.
				m.mu.Unlock()
				m.exposureMu.Unlock()
				continue
			}
			result, err := m.setStoredExposureLocked(clientID, mappingID, exposure)
			m.mu.Unlock()
			m.exposureMu.Unlock()
			return result, err
		}

		// Live path: take the session's control queue before exposureMu. A push
		// can hold controlMu for the ack timeout; waiting for it without
		// exposureMu keeps unrelated registrations from stalling. Registration
		// still takes exposureMu -> m.mu, so it cannot replace this session
		// while the switch releases m.mu around the state fsync.
		if m.setExposureBeforeControlMu != nil {
			m.setExposureBeforeControlMu()
		}
		sess.controlMu.Lock()
		m.exposureMu.Lock()
		m.mu.Lock()
		if m.sessions[clientID] != sess {
			m.mu.Unlock()
			m.exposureMu.Unlock()
			sess.controlMu.Unlock()
			continue
		}
		rt := sess.mappings[mappingID]
		if rt == nil {
			m.mu.Unlock()
			m.exposureMu.Unlock()
			sess.controlMu.Unlock()
			return view.MappingView{}, &MappingNotFoundError{ClientID: clientID, MappingID: mappingID}
		}

		// Disabled, failed, or already-closed mappings have no listener to
		// switch. Store the desired exposure for the next bind.
		var result view.MappingView
		var err error
		switch {
		case rt.closed.Load() || rt.status != mappingStatusRunning || rt.listeners == nil:
			result, err = m.setStoredExposureForRuntimeLocked(sess, rt, exposure)
		case exposureOrDefault(rt.exposure) == exposure && len(rt.listeners.Warnings()) == 0:
			// A healthy same-value request is a read, not a listener rebuild.
			// Keep the retry path when a family warning is present so a repeated
			// request can recover the missing family.
			result = mappingViewForRuntime(rt)
		default:
			result, err = m.switchExposureLocked(sess, rt, exposure)
		}
		m.mu.Unlock()
		m.exposureMu.Unlock()
		if err == nil {
			// Keep controlMu while notifying so the read-only update cannot
			// overtake a push or update ack for the same session.
			m.notifyExposureUpdate(sess, result)
		}
		sess.controlMu.Unlock()
		return result, err
	}
	return view.MappingView{}, control.ErrClientOffline
}

// notifyExposureUpdate sends one exposure_update frame to a client that
// advertised exposure_v1. The caller must hold sess.controlMu and must not hold
// m.mu or exposureMu. Notification is best-effort: the exposure change is
// already committed, and a failed write is corrected by the next hello.
func (m *mappingManager) notifyExposureUpdate(sess *clientSession, result view.MappingView) {
	if sess == nil || !sess.exposureCapability() {
		return
	}
	// Registration publishes the session before hello_ack is written. A switch
	// that wins the race for controlMu must not put exposure_update on the wire
	// first: the client handshake accepts only hello_ack. The ack writer does
	// not need controlMu, so waiting here cannot deadlock. A timed-out wait is
	// safe because the committed exposure is carried by the next HelloAck.
	if err := sess.waitReady(pushReadyTimeout); err != nil {
		m.log.Warn("skip exposure update: control session not ready",
			"client", sess.id,
			"mapping", result.ID,
			"error", err,
		)
		return
	}
	update := &control.ExposureUpdate{
		Type:                control.TypeExposureUpdate,
		ID:                  result.ID,
		Exposure:            string(exposureOrDefault(result.Exposure)),
		UnavailableFamilies: append([]string(nil), result.UnavailableFamilies...),
	}
	if err := sess.write(update); err != nil {
		m.log.Debug("send exposure update",
			"client", sess.id,
			"mapping", result.ID,
			"error", err,
		)
	}
}

// setStoredExposureLocked stores the desired exposure for an offline client.
// The caller must hold m.mu so a concurrent registration cannot bind the old
// value while this transaction persists the new one.
func (m *mappingManager) setStoredExposureLocked(clientID, mappingID string, exposure view.Exposure) (view.MappingView, error) {
	if m.store == nil {
		return view.MappingView{}, control.ErrClientNotFound
	}
	client := m.store.Snapshot().Clients[clientID]
	if client == nil {
		return view.MappingView{}, control.ErrClientNotFound
	}

	var mapping config.Mapping
	found := false
	for _, candidate := range client.Mappings {
		if candidate.ID == mappingID {
			mapping = candidate
			found = true
			break
		}
	}
	if !found {
		return view.MappingView{}, &MappingNotFoundError{ClientID: clientID, MappingID: mappingID}
	}
	previous := exposureOrDefault(view.Exposure(client.Exposures[mappingID]))
	status := mappingStatusOffline
	if !mapping.Enabled {
		status = mappingStatusDisabled
	}
	result := view.MappingView{
		Mapping:      mapping,
		Status:       status,
		AssignedPort: mapping.RemotePort,
		Exposure:     previous,
	}
	if err := m.persistExposure(clientID, mappingID, exposure); err != nil {
		return result, &ExposurePersistError{
			Requested: exposure,
			Exposure:  previous,
			Closed:    true,
			Err:       err,
		}
	}
	result.Exposure = exposure
	return result, nil
}

// setStoredExposureForRuntimeLocked stores the desired exposure for a live
// mapping that currently has no listener. The caller must hold m.mu and the
// session's controlMu.
func (m *mappingManager) setStoredExposureForRuntimeLocked(sess *clientSession, rt *mappingRuntime, exposure view.Exposure) (view.MappingView, error) {
	previous := exposureOrDefault(rt.exposure)
	if err := m.persistExposure(sess.id, rt.id, exposure); err != nil {
		return mappingViewForRuntime(rt), &ExposurePersistError{
			Requested:           exposure,
			Exposure:            previous,
			UnavailableFamilies: mappingWarnings(rt),
			Closed:              rt.listeners == nil,
			Err:                 err,
		}
	}
	rt.exposure = exposure
	return mappingViewForRuntime(rt), nil
}

// persistExposure writes one exposure and treats a post-rename directory sync
// failure as committed, matching state.SetExposure and DeleteAndSave.
func (m *mappingManager) persistExposure(clientID, mappingID string, exposure view.Exposure) error {
	if m == nil || m.store == nil {
		return nil
	}
	err := m.store.SetExposure(clientID, mappingID, string(exposure))
	if err == nil {
		return nil
	}
	if errors.Is(err, state.ErrCommitted) {
		m.log.Warn("mapping exposure committed but state directory sync failed",
			"client", clientID,
			"mapping", mappingID,
			"error", err,
		)
		return nil
	}
	return err
}

// persistExposureTransaction applies the live-switch persistence seam.
func (m *mappingManager) persistExposureTransaction(clientID, mappingID string, exposure view.Exposure) error {
	if m.persistExposureFn != nil {
		return m.persistExposureFn(clientID, mappingID, exposure)
	}
	return m.persistExposure(clientID, mappingID, exposure)
}

// switchExposureLocked performs the live switch transaction. The caller must
// hold m.mu and the session's controlMu. The manager lock is released around
// the state fsync; the caller's m.mu is held again on return.
func (m *mappingManager) switchExposureLocked(sess *clientSession, rt *mappingRuntime, requested view.Exposure) (view.MappingView, error) {
	oldExposure := exposureOrDefault(rt.exposure)
	oldListeners := rt.listeners
	oldPort := rt.assignedPort

	// Retire the old accept generation before closing anything so an accepted
	// but not-yet-tracked public connection cannot be relayed on the new
	// exposure.
	rt.acceptGen.Add(1)

	// Keep UDP attachment serialized with the listener swap. An attachment
	// racing the switch would otherwise capture the socket set being closed.
	rt.udpMu.Lock()

	oldPublic := rt.udpSession
	if oldPublic != nil {
		// Pause the transport session while its public socket is replaced.
		// Closing the wrapper here would tear down the client's reverse UDP
		// session and leave the mapping deaf until a full reconnect.
		oldPublic.beginSwitch()
	}
	if oldListeners != nil {
		_ = oldListeners.Close()
	}
	rt.listeners = nil
	rt.closeActive()
	if rt.data != nil {
		// Pending reverse data streams for connections accepted by the old
		// listeners are no longer valid after the switch. The generation check
		// in take/takeFor also rejects any stream added after this pass.
		rt.data.dropMapping(rt)
	}
	rt.status = mappingStatusPending
	rt.errCode = ""

	newListeners, bindErr := bindMappingListeners(requested, m.bindHost, rt.mapping.Type, oldPort)
	if bindErr != nil {
		result, err := m.rollbackExposureLocked(sess, rt, oldExposure, oldPort, requested, bindErr, false)
		finishUDPWrapperSwitch(oldPublic, rt)
		rt.udpMu.Unlock()
		return result, err
	}

	// The new set is not published yet. Release udpMu before the fsync so an
	// attachment sees nil and returns instead of blocking on disk I/O.
	rt.udpMu.Unlock()

	// Persist outside the manager lock. exposureMu prevents registration from
	// replacing the session; detach/closeAll can still remove it, so re-check
	// the exact session/runtime identity before publishing anything.
	m.mu.Unlock()
	persistErr := m.persistExposureTransaction(sess.id, rt.id, requested)
	m.mu.Lock()

	if m.sessions[sess.id] != sess || sess.mappings[rt.id] != rt {
		_ = newListeners.Close()
		rt.udpMu.Lock()
		rt.listeners = nil
		rt.status = mappingStatusFailed
		rt.errCode = string(control.CodeInternal)
		rt.exposure = oldExposure
		finishUDPWrapperSwitch(oldPublic, rt)
		rt.udpMu.Unlock()
		result := mappingViewForRuntime(rt)
		if persistErr != nil {
			return result, &ExposureSwitchError{
				Requested:   requested,
				Exposure:    oldExposure,
				Closed:      true,
				Persistence: true,
				Err:         persistErr,
			}
		}
		return result, &ExposureSwitchError{
			Requested: requested,
			Exposure:  oldExposure,
			Closed:    true,
			Err:       control.ErrClientOffline,
		}
	}

	if persistErr != nil {
		// Close the bound-but-unpublished set before rebinding the old
		// exposure; otherwise a wildcard rollback can conflict with the new
		// loopback sockets on the same port.
		_ = newListeners.Close()
		rt.udpMu.Lock()
		result, err := m.rollbackExposureLocked(sess, rt, oldExposure, oldPort, requested, persistErr, true)
		finishUDPWrapperSwitch(oldPublic, rt)
		rt.udpMu.Unlock()
		return result, err
	}

	// Publish only after persistence and the identity re-check succeeded.
	rt.udpMu.Lock()
	rt.listeners = newListeners
	rt.assignedPort = newListeners.Port()
	rt.exposure = requested
	rt.status = mappingStatusRunning
	rt.errCode = ""
	if oldPublic != nil && rt.udpSession == oldPublic {
		oldPublic.endSwitch(newListeners.UDP)
	}
	rt.udpMu.Unlock()
	if rt.mapping.Type == "tcp" {
		m.startTCPListenersLocked(sess, rt, newListeners)
	}
	return mappingViewForRuntime(rt), nil
}

// finishUDPWrapperSwitch resumes a paused reverse UDP session on the listener
// set that is actually active after a switch or rollback. If no UDP listener
// remains, the wrapper is closed so the client's session fails instead of
// waiting forever. The caller must hold rt.udpMu.
func finishUDPWrapperSwitch(public *publicUDPConn, rt *mappingRuntime) {
	if public == nil {
		return
	}
	if rt.listeners != nil && rt.listeners.UDP != nil {
		public.endSwitch(rt.listeners.UDP)
		return
	}
	public.abortSwitch()
	_ = public.Close()
	if rt.udpSession == public {
		rt.udpSession = nil
	}
}

// rollbackExposureLocked restores the previous exposure after a failed switch.
// If the restore also fails the mapping stays closed and the error reports the
// actual state instead of pretending the old listeners are active. persistence
// classifies the original failure for the Web API (409 vs 500).
func (m *mappingManager) rollbackExposureLocked(sess *clientSession, rt *mappingRuntime, oldExposure view.Exposure, port int, requested view.Exposure, cause error, persistence bool) (view.MappingView, error) {
	restored, restoreErr := bindMappingListeners(oldExposure, m.bindHost, rt.mapping.Type, port)
	if restoreErr != nil {
		rt.listeners = nil
		rt.status = mappingStatusFailed
		rt.errCode = string(control.CodeInternal)
		rt.exposure = oldExposure
		m.log.Warn("mapping exposure switch failed and rollback bind failed",
			"client", sess.id,
			"mapping", rt.id,
			"requested", requested,
			"active", oldExposure,
			"error", errors.Join(cause, restoreErr),
		)
		return mappingViewForRuntime(rt), &ExposureSwitchError{
			Requested:   requested,
			Exposure:    oldExposure,
			Closed:      true,
			Persistence: persistence,
			Err:         errors.Join(cause, restoreErr),
		}
	}

	rt.listeners = restored
	rt.assignedPort = restored.Port()
	rt.status = mappingStatusRunning
	rt.errCode = ""
	rt.exposure = oldExposure
	if rt.mapping.Type == "tcp" {
		m.startTCPListenersLocked(sess, rt, restored)
	}
	m.log.Warn("mapping exposure switch rolled back",
		"client", sess.id,
		"mapping", rt.id,
		"requested", requested,
		"active", oldExposure,
		"error", cause,
	)
	return mappingViewForRuntime(rt), &ExposureSwitchError{
		Requested:           requested,
		Exposure:            oldExposure,
		UnavailableFamilies: restored.Warnings(),
		Persistence:         persistence,
		Err:                 cause,
	}
}

// startTCPListenersLocked starts one accept loop per bound family. It captures
// the current accept generation once so every listener in the set retires
// together. It is called after the new listeners are persisted, so a failed
// switch never accepts traffic on a listener set that was rolled back.
func (m *mappingManager) startTCPListenersLocked(sess *clientSession, rt *mappingRuntime, listeners *mappingListenerSet) {
	if listeners == nil {
		return
	}
	generation := rt.acceptGen.Load()
	for _, listener := range listeners.TCP {
		if m.serveTCP != nil {
			go m.serveTCP(sess, rt, listener, generation)
		} else {
			go acceptAndCloseTCP(listener)
		}
	}
}
