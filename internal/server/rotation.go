package server

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/credentials"
	"hy2tun/internal/view"
)

// Stable credential rotation error codes. The web API maps them to 400
// responses without parsing human-readable messages.
var (
	ErrUnsupportedAuthType = errors.New("unsupported_auth_type")
	ErrNoCredentialChange  = errors.New("no_change")
)

// credentialAckTimeout bounds how long a rotation waits for one client's
// credential_ack. It is a variable so tests can shorten the wait.
var credentialAckTimeout = 5 * time.Second

const (
	credentialStatusSynced      = "synced"
	credentialStatusPending     = "pending"
	credentialStatusUnsupported = "unsupported"
	credentialStatusTimeout     = "timeout"
	credentialStatusUnknown     = "unknown"
)

// RotateCredentials replaces the current HY2 password and/or control token,
// persists the new generation, swaps the runtime credential set, and optionally
// pushes the new pair to online capable clients.
//
// Blank arguments keep the current value. If neither value changes, the call
// returns ErrNoCredentialChange and does not create a new generation. The
// previous pair remains valid until RevokePreviousCredentials.
func (s *Server) RotateCredentials(auth, controlToken, syncMode string) (int64, []view.CredentialResult, error) {
	if s == nil {
		return 0, nil, fmt.Errorf("rotate credentials: server is nil")
	}

	// Serialize rotations so two requests cannot interleave generation
	// increments or auto-push waits.
	s.rotationMu.Lock()
	defer s.rotationMu.Unlock()

	current := s.currentCredentials()
	if current == nil {
		return 0, nil, fmt.Errorf("rotate credentials: credential store is empty")
	}

	s.settingsMu.Lock()
	cfg := s.cfg
	if cfg == nil {
		s.settingsMu.Unlock()
		return 0, nil, fmt.Errorf("rotate credentials: server config is nil")
	}
	if cfg.Auth.Type != "password" {
		s.settingsMu.Unlock()
		return 0, nil, ErrUnsupportedAuthType
	}

	currentGeneration := current.Generation
	if currentGeneration < 1 {
		currentGeneration = 1
	}
	nextAuth := current.Auth
	if auth != "" {
		nextAuth = auth
	}
	nextToken := current.ControlToken
	if controlToken != "" {
		nextToken = controlToken
	}
	// No-op checks run before length validation. A historical value may be
	// shorter than the current minimum, and an unchanged field must not be
	// revalidated when only the other field rotates.
	if nextAuth == current.Auth && nextToken == current.ControlToken {
		s.settingsMu.Unlock()
		return 0, nil, ErrNoCredentialChange
	}
	if nextAuth != current.Auth && len(nextAuth) < 8 {
		s.settingsMu.Unlock()
		return 0, nil, fmt.Errorf("auth password must be at least 8 bytes")
	}
	if nextToken != current.ControlToken && len(nextToken) < 16 {
		s.settingsMu.Unlock()
		return 0, nil, fmt.Errorf("control token must be at least 16 bytes")
	}

	nextGeneration := currentGeneration + 1
	next := cloneConfig(cfg)
	next.Auth.Password = nextAuth
	next.Control.Token = nextToken
	next.Credentials = config.CredentialsConfig{
		Generation:           nextGeneration,
		PreviousGeneration:   currentGeneration,
		PreviousAuth:         current.Auth,
		PreviousControlToken: current.ControlToken,
	}
	if err := next.Validate(); err != nil {
		s.settingsMu.Unlock()
		return 0, nil, fmt.Errorf("rotate credentials: %w", err)
	}
	// Persist before touching runtime state. A failed write leaves the old
	// credentials and generation in effect.
	if err := s.persistConfig(next); err != nil {
		s.settingsMu.Unlock()
		return 0, nil, fmt.Errorf("rotate credentials: %w", err)
	}

	newSet := &credentials.Set{
		Generation:           nextGeneration,
		Auth:                 nextAuth,
		ControlToken:         nextToken,
		PreviousGeneration:   currentGeneration,
		PreviousAuth:         current.Auth,
		PreviousControlToken: current.ControlToken,
	}
	s.cfg = next
	s.credentials.Store(newSet)
	s.settingsMu.Unlock()

	// A second rotation supersedes any generation older than the new previous
	// pair. Those clients can no longer authenticate after a reconnect, so
	// close their control streams now instead of leaving them online.
	s.closeSupersededSessions(nextGeneration, currentGeneration)
	// credentialSynced is a Hello-time snapshot. Refresh it for every session
	// so manual rotations and failed pushes show up as pending immediately.
	s.refreshCredentialSync(nextGeneration)

	results := s.syncCredentials(nextGeneration, currentGeneration, nextAuth, nextToken, syncMode)
	s.log.Info("credentials rotated", "generation", nextGeneration)
	return nextGeneration, results, nil
}

// RevokePreviousCredentials clears the retained previous pair. After the
// configuration is written, only the current pair is accepted and every
// session that is not on the current generation is disconnected.
func (s *Server) RevokePreviousCredentials() error {
	if s == nil {
		return fmt.Errorf("revoke previous credentials: server is nil")
	}

	s.rotationMu.Lock()
	defer s.rotationMu.Unlock()

	current := s.currentCredentials()
	if current == nil || current.PreviousGeneration == 0 {
		return nil
	}
	currentGeneration := current.Generation
	if currentGeneration < 1 {
		currentGeneration = 1
	}

	s.settingsMu.Lock()
	cfg := s.cfg
	if cfg == nil {
		s.settingsMu.Unlock()
		return fmt.Errorf("revoke previous credentials: server config is nil")
	}
	next := cloneConfig(cfg)
	next.Auth.Password = current.Auth
	next.Control.Token = current.ControlToken
	next.Credentials = config.CredentialsConfig{Generation: currentGeneration}
	if err := next.Validate(); err != nil {
		s.settingsMu.Unlock()
		return fmt.Errorf("revoke previous credentials: %w", err)
	}
	if err := s.persistConfig(next); err != nil {
		s.settingsMu.Unlock()
		return fmt.Errorf("revoke previous credentials: %w", err)
	}

	newSet := &credentials.Set{
		Generation:   currentGeneration,
		Auth:         current.Auth,
		ControlToken: current.ControlToken,
	}
	s.cfg = next
	s.credentials.Store(newSet)
	s.settingsMu.Unlock()

	s.closeSessionsNotCurrent(currentGeneration)
	s.log.Info("previous credentials revoked", "generation", currentGeneration)
	return nil
}

// CredentialsSnapshot returns an independent copy of the live credential set.
// It contains secrets, so callers must only expose it through an authorized,
// no-store endpoint.
func (s *Server) CredentialsSnapshot() *credentials.Set {
	if s == nil {
		return nil
	}
	return s.currentCredentials()
}

// CredentialStatuses returns the credential state of every online client.
// Generation is the client's reported generation; Status is one of synced,
// pending, unsupported, or unknown.
func (s *Server) CredentialStatuses() []view.CredentialResult {
	if s == nil || s.mappings == nil {
		return nil
	}
	current := s.currentCredentials()
	sessions := s.mappings.sessionSnapshot()
	out := make([]view.CredentialResult, 0, len(sessions))
	for _, entry := range sessions {
		generation, synced, capable := entry.sess.credentialState()
		status := credentialStatusUnknown
		switch {
		case current != nil && generation == current.Generation && synced:
			status = credentialStatusSynced
		case current != nil && generation == current.Generation:
			// The generation matches but the session has not confirmed it.
			status = credentialStatusPending
		case !capable:
			status = credentialStatusUnsupported
		case current != nil && current.PreviousGeneration > 0 && generation == current.PreviousGeneration:
			status = credentialStatusPending
		}
		out = append(out, view.CredentialResult{
			ClientID:   entry.id,
			Name:       entry.name,
			Generation: generation,
			Status:     status,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ClientID < out[j].ClientID })
	return out
}

// closeSupersededSessions disconnects sessions whose generation is neither the
// new current nor the new previous generation.
func (s *Server) closeSupersededSessions(currentGeneration, previousGeneration int64) {
	if s == nil || s.mappings == nil {
		return
	}
	for _, entry := range s.mappings.sessionSnapshot() {
		generation, _, _ := entry.sess.credentialState()
		if generation != currentGeneration && generation != previousGeneration {
			entry.sess.abortStream()
		}
	}
}

// closeSessionsNotCurrent disconnects every session that is not on the current
// generation. Revoke uses it because the previous generation is gone.
func (s *Server) closeSessionsNotCurrent(currentGeneration int64) {
	if s == nil || s.mappings == nil {
		return
	}
	for _, entry := range s.mappings.sessionSnapshot() {
		generation, _, _ := entry.sess.credentialState()
		if generation != currentGeneration {
			entry.sess.abortStream()
		}
	}
}

// refreshCredentialSync recomputes the synced flag from each session's reported
// generation. It runs after a rotation so views and status results do not keep
// the Hello-time value.
func (s *Server) refreshCredentialSync(currentGeneration int64) {
	if s == nil || s.mappings == nil {
		return
	}
	for _, entry := range s.mappings.sessionSnapshot() {
		generation, _, capable := entry.sess.credentialState()
		entry.sess.setCredentialState(generation, generation == currentGeneration, capable)
	}
}

// syncCredentials builds one result per online session. In auto mode it pushes
// the new pair to ready, capable sessions concurrently; each push has its own
// timeout so one silent client does not delay the others.
func (s *Server) syncCredentials(generation, previousGeneration int64, auth, controlToken, syncMode string) []view.CredentialResult {
	if s == nil || s.mappings == nil {
		return nil
	}
	sessions := s.mappings.sessionSnapshot()
	results := make([]view.CredentialResult, len(sessions))
	var wg sync.WaitGroup

	for i, entry := range sessions {
		result := view.CredentialResult{
			ClientID:   entry.id,
			Name:       entry.name,
			Generation: generation,
			Status:     credentialStatusPending,
		}
		sessionGeneration, synced, capable := entry.sess.credentialState()

		switch {
		case sessionGeneration == generation && synced:
			result.Status = credentialStatusSynced
		case sessionGeneration != generation && sessionGeneration != previousGeneration:
			// The session is older than the retained previous pair and was
			// disconnected above.
			result.Error = "superseded"
		case sessionGeneration == generation:
			// The client claims the new generation but has not confirmed it.
		case syncMode != "auto":
			// Manual sync leaves every online client pending.
		case !entry.sess.isReady():
			result.Error = "not_ready"
		case !capable:
			result.Status = credentialStatusUnsupported
		default:
			results[i] = result
			wg.Add(1)
			go func(index int, session sessionEntry) {
				defer wg.Done()
				results[index] = s.pushCredentials(session.sess, session.id, session.name, generation, auth, controlToken)
			}(i, entry)
			continue
		}
		results[i] = result
	}

	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].ClientID < results[j].ClientID })
	return results
}

// pushCredentials sends one credential_update and waits for a matching ack.
// Stale acks are drained and filtered by generation so a timed-out earlier push
// cannot satisfy this one.
func (s *Server) pushCredentials(sess *clientSession, clientID, name string, generation int64, auth, controlToken string) view.CredentialResult {
	result := view.CredentialResult{
		ClientID:   clientID,
		Name:       name,
		Generation: generation,
		Status:     credentialStatusPending,
	}

	sess.setCredentialWait(generation)
	defer sess.setCredentialWait(0)
	sess.drainCredentialAcks()

	// The write and the ack wait share one budget. A client that stops reading
	// must not be able to hold rotationMu for the 10s generic write deadline
	// plus the 5s ack timeout.
	deadline := time.Now().Add(credentialAckTimeout)
	update := &control.CredentialUpdate{
		Type:         control.TypeCredentialUpdate,
		Generation:   generation,
		Auth:         auth,
		ControlToken: controlToken,
	}
	if err := sess.writeWithDeadline(update, deadline); err != nil {
		if !time.Now().Before(deadline) {
			result.Status = credentialStatusTimeout
			result.Error = "ack_timeout"
		} else {
			result.Error = "write_failed"
		}
		return result
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		result.Status = credentialStatusTimeout
		result.Error = "ack_timeout"
		return result
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	for {
		select {
		case ack := <-sess.credentialAck:
			if ack == nil || ack.Generation != generation {
				continue
			}
			if ack.OK {
				_, _, capable := sess.credentialState()
				sess.setCredentialState(generation, true, capable)
				result.Status = credentialStatusSynced
				// The client has the new pair on disk. Close the old control
				// stream so it reconnects with the new credentials instead of
				// keeping a session on the previous generation.
				sess.abortStream()
				return result
			}
			result.Status = credentialStatusPending
			result.Error = ack.Error
			if result.Error == "" {
				result.Error = "ack_failed"
			}
			return result
		case <-sess.done:
			result.Error = "session_closed"
			return result
		case <-timer.C:
			result.Status = credentialStatusTimeout
			result.Error = "ack_timeout"
			return result
		}
	}
}
