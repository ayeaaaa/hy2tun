package server

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"time"

	"hy2tun/internal/control"
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
	if conn == nil {
		return
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(controlHandshakeTimeout))
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
	if _, ok := set.AcceptControlToken(hello.Token); !ok {
		h.reject(conn, control.CodeUnauthorized)
		return
	}
	if hello.ClientID == "" {
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
	synced := set.Synced(generation)
	capable := hasCapability(hello.Capabilities, control.CapabilityCredentialUpdate)

	sess := &clientSession{
		id:              hello.ClientID,
		name:            hello.Name,
		version:         hello.Version,
		revision:        hello.Revision,
		appliedRevision: hello.Revision,
		credentialAck:   make(chan *control.CredentialAck, 1),
		conn:            conn,
		ready:           make(chan struct{}),
		done:            make(chan struct{}),
	}
	sess.setCredentialState(generation, synced, capable)
	results := h.server.mappings.register(sess, hello.Mappings)

	ack := &control.HelloAck{
		Type:                 control.TypeHelloAck,
		OK:                   true,
		ServerVersion:        version.Version,
		CredentialGeneration: set.Generation,
		CredentialSynced:     synced,
		Results:              results,
	}
	if err := sess.write(ack); err != nil {
		h.server.mappings.detach(sess)
		return
	}
	sess.markReady()

	h.log.Info("hy2tun client registered",
		"client", hello.ClientID,
		"name", hello.Name,
		"mappings", len(hello.Mappings),
	)
	h.loop(sess)
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
	defer h.server.mappings.detach(sess)

	for {
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
			select {
			case sess.credentialAck <- message:
			default:
			}
		case *control.Ping:
			if err := sess.write(&control.Pong{Type: control.TypePong, TS: message.TS}); err != nil {
				return
			}
		case *control.OpenResult:
			if !message.OK && h.server.data != nil {
				h.server.data.fail(message.Token)
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
	results, accepted, resync := h.server.mappings.update(sess, message.Revision, message.Mappings)
	if accepted && message.Name != "" {
		// A rename is a separate metadata write. A nil mapping list was left
		// untouched by updateLocked, while an explicit list was already
		// applied above.
		h.server.mappings.rename(sess, message.Name)
	}
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
