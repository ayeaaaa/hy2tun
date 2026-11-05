package server

import (
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net"
	"time"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/transport"
	"hy2tun/internal/version"
)

const controlHandshakeTimeout = 10 * time.Second

// controlHandler serves one hy2tun control stream. Each stream runs on one end
// of a net.Pipe created by the outbound router.
type controlHandler struct {
	server *Server
	log    *slog.Logger
}

func newControlHandler(s *Server, log *slog.Logger) *controlHandler {
	if log == nil {
		log = slog.Default()
	}
	return &controlHandler{server: s, log: log}
}

// Serve authenticates a client, registers its mappings, and then processes
// control messages until the stream closes.
func (h *controlHandler) Serve(conn net.Conn) {
	h.serveWithMeta(conn, sessionMeta{Kind: transport.HY2})
}

func (h *controlHandler) serveWithMeta(conn net.Conn, meta sessionMeta) {
	if conn == nil {
		return
	}
	defer conn.Close()
	if meta.CloseTransport != nil {
		defer meta.CloseTransport()
	}

	deadline := meta.HandshakeDeadline
	if deadline.IsZero() {
		deadline = time.Now().Add(controlHandshakeTimeout)
	}
	_ = conn.SetReadDeadline(deadline)
	msg, err := control.ReadMessage(conn)
	if err != nil {
		if errors.Is(err, control.ErrBadMessage) ||
			errors.Is(err, control.ErrUnknownType) ||
			errors.Is(err, control.ErrFrameTooLarge) {
			h.reject(conn, control.CodeBadMessage)
		}
		h.log.Debug("read control hello", "error", err)
		return
	}

	hello, ok := msg.(*control.Hello)
	if !ok {
		h.reject(conn, control.CodeBadMessage)
		return
	}
	if hello.Protocol != control.ProtocolVersion {
		h.reject(conn, control.CodeUnsupportedProtocol)
		return
	}
	set := h.server.currentCredentials()
	authenticatedGeneration, ok := set.AcceptControlToken(hello.Token)
	if !ok {
		h.reject(conn, control.CodeUnauthorized)
		return
	}
	if meta.Kind == transport.TCP {
		if meta.Principal == nil || meta.Peer == nil || hello.ClientID != meta.Principal.ClientID || sha256.Sum256([]byte(hello.Token)) != meta.Principal.TokenDigest || !tcpPrincipalValid(*meta.Principal, set) || !h.server.currentConfig().Mode.AllowsTCP() {
			h.reject(conn, control.CodeUnauthorized)
			return
		}
		authenticatedGeneration = meta.Principal.Generation
	}
	if hello.ClientID == "" {
		h.reject(conn, control.CodeBadMessage)
		return
	}
	if len(hello.Mappings) > maxMappingsPerClient {
		h.reject(conn, control.CodeBadMessage)
		return
	}
	if hello.Revision < 0 {
		h.reject(conn, control.CodeBadMessage)
		return
	}
	if hello.CredentialGeneration < 0 {
		h.reject(conn, control.CodeBadMessage)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	// A missing generation means a legacy client that predates credential
	// rotation. Treat it as the initial generation so it reports as synced
	// until the first rotation.
	generation := hello.CredentialGeneration
	if generation == 0 {
		generation = 1
	}
	if authenticatedGeneration < 1 {
		authenticatedGeneration = 1
	}
	// Sync state follows the generation that actually authenticated the
	// control token, not the generation the client claims.
	synced := set.Synced(authenticatedGeneration)
	capable := hasCapability(hello.Capabilities, control.CapabilityCredentialUpdate)

	sess := &clientSession{
		id:              hello.ClientID,
		name:            hello.Name,
		version:         hello.Version,
		revision:        hello.Revision,
		appliedRevision: hello.Revision,
		credentialAck:   make(chan *control.CredentialAck, 1),
		conn:            conn,
		transport:       meta.Kind,
		tcpPeer:         meta.Peer,
		ready:           make(chan struct{}),
		done:            make(chan struct{}),
	}
	sess.setCredentialStateFor(generation, authenticatedGeneration, synced, capable)
	results, err := h.server.registerControl(sess, hello, meta, deadline, generation, capable)
	if err != nil {
		return
	}

	// Registration races revoke and rotation sweeps: a hello that authenticated
	// just before the credential set changed could otherwise register after the
	// sweep and keep an old token alive. Re-check the token against the current
	// set and refresh the authenticated state before acknowledging.
	current := h.server.currentCredentials()
	if current == nil {
		h.server.mappings.detach(sess)
		return
	}
	authenticatedGeneration, ok = current.AcceptControlToken(hello.Token)
	if !ok {
		h.server.mappings.detach(sess)
		return
	}
	if meta.Kind == transport.TCP {
		if !tcpPrincipalValid(*meta.Principal, current) || !h.server.currentConfig().Mode.AllowsTCP() {
			h.server.mappings.detach(sess)
			return
		}
		authenticatedGeneration = meta.Principal.Generation
	}
	if authenticatedGeneration < 1 {
		authenticatedGeneration = 1
	}
	sess.setCredentialStateFor(generation, authenticatedGeneration, current.Synced(authenticatedGeneration), capable)

	ack := &control.HelloAck{
		Type:                 control.TypeHelloAck,
		OK:                   true,
		ServerVersion:        version.Version,
		CredentialGeneration: current.Generation,
		CredentialSynced:     current.Synced(authenticatedGeneration),
		Results:              results,
	}
	ackDeadline := time.Time{}
	if meta.Kind == transport.TCP {
		ackDeadline = deadline
	}
	if err := sess.writeWithDeadline(ack, ackDeadline); err != nil {
		h.server.mappings.detach(sess)
		return
	}
	sess.markReady()
	if meta.registered != nil {
		meta.registered()
	}

	h.log.Info("hy2tun client registered",
		"client", hello.ClientID,
		"name", hello.Name,
		"mappings", len(hello.Mappings),
	)
	h.loop(sess)
}

// acquireAdmission acquires the mapping manager lock and a settings read lock,
// returning the configuration snapshot protected by that read lock. TryRLock is
// attempted under the manager lock so a settings writer (mode or credential
// swap) cannot deadlock behind a waiting Hello; if a writer is active, the
// manager lock is released and a blocking read waits for that writer before the
// retry. The caller must release settingsMu and then mappings.mu.
func (s *Server) acquireAdmission() *config.ServerConfig {
	m := s.mappings
	for {
		m.mu.Lock()
		if s.settingsMu.TryRLock() {
			return s.cfg
		}
		m.mu.Unlock()
		// Wait for the active settings writer without holding the manager
		// lock. Reading the configuration under the blocking read lock makes
		// the barrier a real critical section; the retry re-reads it under the
		// final lock.
		_ = s.currentConfig()
	}
}

// registerControl validates and commits admission atomically before an existing
// owner can be replaced. Writers use settingsMu for mode/credential swaps. TryRLock
// avoids the settings-writer -> manager lock order used by hot port updates, and
// does not prevent revocation while this Hello waits for the manager.
func (s *Server) registerControl(sess *clientSession, hello *control.Hello, meta sessionMeta, deadline time.Time, reported int64, capable bool) ([]control.Result, error) {
	settings := s.acquireAdmission()
	m := s.mappings
	var old *clientSession
	commit := func() error {
		current := s.currentCredentials()
		authenticated, ok := current.AcceptControlToken(hello.Token)
		if !ok || s.closed.Load() || !time.Now().Before(deadline) {
			return transport.ErrAuthentication
		}
		if meta.endpointClosed != nil && meta.endpointClosed.Load() {
			return transport.ErrAuthentication
		}
		if meta.Kind == transport.HY2 && (settings == nil || !settings.Mode.AllowsHY2()) {
			return transport.ErrAuthentication
		}
		if meta.Kind == transport.TCP {
			if meta.Principal == nil || meta.Peer == nil || meta.Peer.Principal() != *meta.Principal || hello.ClientID != meta.Principal.ClientID || sha256.Sum256([]byte(hello.Token)) != meta.Principal.TokenDigest || !tcpPrincipalValid(*meta.Principal, current) || settings == nil || !settings.Mode.AllowsTCP() {
				return transport.ErrAuthentication
			}
			authenticated = meta.Principal.Generation
		}
		if m.sessions[sess.id] == nil && len(m.sessions) >= maxControlClients {
			return transport.ErrAuthentication
		}
		sess.setCredentialStateFor(reported, authenticated, current.Synced(authenticated), capable)
		old = m.admitSessionLocked(sess, time.Now())
		return nil
	}
	var err error
	if meta.Kind == transport.TCP {
		if meta.Peer == nil {
			err = transport.ErrAuthentication
		} else {
			err = meta.Peer.WithLive(commit)
		}
	} else {
		err = commit()
	}
	var results []control.Result
	if err == nil {
		// The short root guard is released before resource cleanup/binding. The
		// manager still hides partial registration; root timeout/Close can proceed.
		results = m.finishRegistrationLocked(sess, old, hello.Mappings, time.Now())
	}
	s.settingsMu.RUnlock()
	m.mu.Unlock()
	if err == nil {
		m.saveState()
	}
	return results, err
}

// hasCapability reports whether capabilities advertises want.
func hasCapability(capabilities []string, want string) bool {
	for _, capability := range capabilities {
		if capability == want {
			return true
		}
	}
	return false
}

// reject sends a failed HelloAck with the given stable error code and closes
// the stream when Serve returns.
func (h *controlHandler) reject(conn net.Conn, code control.ErrorCode) {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = control.WriteMessage(conn, &control.HelloAck{
		Type:  control.TypeHelloAck,
		OK:    false,
		Error: string(code),
	})
}

func (h *controlHandler) loop(sess *clientSession) {
	h.loopWithHeartbeat(sess, 10*time.Second, 30*time.Second)
}

func (h *controlHandler) loopWithHeartbeat(sess *clientSession, interval, idle time.Duration) {
	defer h.server.mappings.detach(sess)
	stop := make(chan struct{})
	defer close(stop)
	if sess.transport == transport.TCP {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-sess.done:
					return
				case now := <-ticker.C:
					if err := sess.write(&control.Ping{Type: control.TypePing, TS: now.UnixMilli()}); err != nil {
						sess.abortStream()
						return
					}
				}
			}
		}()
	}

	for {
		if sess.transport == transport.TCP {
			_ = sess.conn.SetReadDeadline(time.Now().Add(idle))
		}
		msg, err := control.ReadMessage(sess.conn)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				h.log.Debug("control stream ended", "client", sess.id, "error", err)
			}
			return
		}

		switch message := msg.(type) {
		case *control.Update:
			// Process the update outside the read loop. The push transaction
			// holds controlMu while waiting for its ack, so blocking here would
			// prevent the ack from ever being read.
			go h.handleUpdate(sess, message)
		case *control.PushAck:
			if !sess.deliverPushAck(message) {
				h.log.Debug("unexpected control push ack",
					"client", sess.id,
					"ok", message.OK,
					"revision", message.Revision,
				)
			}
		case *control.CredentialAck:
			// Rotation waits on this channel. Forward the whole ack
			// non-blockingly so the waiter can distinguish success from
			// failure; the client reports the generation it applied, which can
			// be newer than the generation this session registered with.
			sessionGeneration, _, _ := sess.credentialState()
			if message.Generation != sessionGeneration {
				h.log.Debug("credential ack generation differs from session",
					"client", sess.id,
					"generation", message.Generation,
					"session_generation", sessionGeneration,
					"ok", message.OK,
				)
			}
			if !sess.deliverCredentialAck(message) {
				h.log.Debug("ignoring unexpected credential ack",
					"client", sess.id,
					"generation", message.Generation,
					"ok", message.OK,
				)
			}
		case *control.Ping:
			if err := sess.write(&control.Pong{Type: control.TypePong, TS: message.TS}); err != nil {
				return
			}
		case *control.Pong:
			// Reading any valid control response refreshes the TCP idle deadline.
		case *control.OpenResult:
			if !message.OK && h.server.data != nil {
				if sess.transport == transport.TCP {
					h.server.data.failFor(message.Token, sess)
				} else {
					h.server.data.fail(message.Token)
				}
			}
			h.log.Debug("received control open result",
				"client", sess.id,
				"ok", message.OK,
				"error", message.Error,
			)
		default:
			h.log.Debug("ignoring unexpected control message", "client", sess.id)
		}
	}
}

// handleUpdate applies one client update while holding the per-client control
// queue. Updates are revision-checked, so out-of-order execution cannot apply
// an older list over a newer one.
func (h *controlHandler) handleUpdate(sess *clientSession, message *control.Update) {
	sess.controlMu.Lock()
	// A non-empty name makes a nil mapping list metadata-only. A nil list with
	// no name is an explicit clear from an old client whose mapping slice is
	// empty.
	results, accepted, resync := h.server.mappings.updateWithName(sess, message.Revision, message.Mappings, message.Name)
	ack := &control.UpdateAck{
		Type:     control.TypeUpdateAck,
		Revision: h.server.mappings.sessionRevision(sess),
		Results:  results,
	}
	// Write the ack before releasing the per-client queue so a push cannot
	// overtake the update that produced it.
	err := sess.write(ack)
	if resync {
		// The client's full configuration is authoritative. Its reconnect
		// re-registers every mapping, which closes removed or disabled
		// listeners that the rejected transaction left untouched.
		h.log.Warn("closing control stream after rejected update",
			"client", sess.id,
			"revision", message.Revision,
		)
		sess.abortStream()
	}
	sess.controlMu.Unlock()

	if !accepted {
		h.log.Debug("rejected control update",
			"client", sess.id,
			"revision", message.Revision,
		)
	}
	if err != nil {
		h.log.Debug("write control update ack", "client", sess.id, "error", err)
	}
}

// setPushWaiter registers the channel that receives the next matching PushAck.
// Only one push may be in flight per session because the caller holds
// controlMu. The expected push ID and revision filter out late acks from a
// previous timed-out push before they reach the channel.
func (s *clientSession) setPushWaiter(ch chan *control.PushAck, pushID string, revision int64) {
	s.pushWaitMu.Lock()
	s.pushWait = ch
	s.pushWaitID = pushID
	s.pushWaitRev = revision
	s.pushWaitMu.Unlock()
}

// clearPushWaiter removes ch if it is still the active waiter. A late ack
// after a timeout is then ignored.
func (s *clientSession) clearPushWaiter(ch chan *control.PushAck) {
	s.pushWaitMu.Lock()
	if s.pushWait == ch {
		s.pushWait = nil
		s.pushWaitID = ""
		s.pushWaitRev = 0
	}
	s.pushWaitMu.Unlock()
}

// deliverPushAck forwards a matching ack to the waiting push without blocking
// the control read loop. Acks for another push are dropped so they cannot fill
// the waiter's channel and hide the matching ack.
func (s *clientSession) deliverPushAck(ack *control.PushAck) bool {
	s.pushWaitMu.Lock()
	ch := s.pushWait
	pushID := s.pushWaitID
	revision := s.pushWaitRev
	s.pushWaitMu.Unlock()
	if ch == nil || !pushAckMatches(ack, pushID, revision) {
		return false
	}
	select {
	case ch <- ack:
		return true
	default:
		return false
	}
}
