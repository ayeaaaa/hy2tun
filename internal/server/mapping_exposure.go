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
// listener remains. Persistence distinguishes a state-save failure (500) from
// a bind/rollback conflict (409). It never contains secret material.
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

// ExposureConflict marks a bind/rollback conflict for cross-package
// classification. Persistence failures return false so the API can map them to
// 500 rather than 409.
func (e *ExposureSwitchError) ExposureConflict() bool {
	return e != nil && !e.Persistence
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

// mappingViewForRuntime builds the read model for a live runtime.
func mappingViewForRuntime(rt *mappingRuntime) view.MappingView {
	if rt == nil {
		return view.MappingView{}
	}
	var unavailable []string
	if rt.listeners != nil {
		unavailable = rt.listeners.Warnings()
	}
	return view.MappingView{
		Mapping:             rt.mapping,
		Status:              rt.status,
		Error:               rt.errCode,
		AssignedPort:        rt.assignedPort,
		Exposure:            exposureOrDefault(rt.exposure),
		UnavailableFamilies: unavailable,
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
		if rt.closed.Load() || rt.status != mappingStatusRunning || rt.listeners == nil {
			result, err := m.setStoredExposureForRuntimeLocked(sess, rt, exposure)
			m.mu.Unlock()
			m.exposureMu.Unlock()
			sess.controlMu.Unlock()
			return result, err
		}

		// A healthy same-value request is a read, not a listener rebuild. Keep
		// the retry path when a family warning is present so a repeated
		// request can recover the missing family.
		if exposureOrDefault(rt.exposure) == exposure && len(rt.listeners.Warnings()) == 0 {
			result := mappingViewForRuntime(rt)
			m.mu.Unlock()
			m.exposureMu.Unlock()
			sess.controlMu.Unlock()
			return result, nil
		}

		result, err := m.switchExposureLocked(sess, rt, exposure)
		m.mu.Unlock()
		m.exposureMu.Unlock()
		sess.controlMu.Unlock()
		return result, err
	}
	return view.MappingView{}, control.ErrClientOffline
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
	if err := m.persistExposure(clientID, mappingID, exposure); err != nil {
		return view.MappingView{}, fmt.Errorf("persist mapping exposure: %w", err)
	}

	status := mappingStatusOffline
	if !mapping.Enabled {
		status = mappingStatusDisabled
	}
	return view.MappingView{
		Mapping:      mapping,
		Status:       status,
		AssignedPort: mapping.RemotePort,
		Exposure:     exposure,
	}, nil
}

// setStoredExposureForRuntimeLocked stores the desired exposure for a live
// mapping that currently has no listener. The caller must hold m.mu and the
// session's controlMu.
func (m *mappingManager) setStoredExposureForRuntimeLocked(sess *clientSession, rt *mappingRuntime, exposure view.Exposure) (view.MappingView, error) {
	if err := m.persistExposure(sess.id, rt.id, exposure); err != nil {
		return mappingViewForRuntime(rt), fmt.Errorf("persist mapping exposure: %w", err)
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
	rt.udpSession = nil
	if oldPublic != nil {
		// Detach and drain the old HY2 UDP wrapper without waiting for its
		// bridge while the manager lock is held; closing the old listeners
		// below already wakes its blocked read.
		go func() { _ = oldPublic.Close() }()
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
	rt.udpMu.Unlock()
	if rt.mapping.Type == "tcp" {
		m.startTCPListenersLocked(sess, rt, newListeners)
	}
	return mappingViewForRuntime(rt), nil
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
