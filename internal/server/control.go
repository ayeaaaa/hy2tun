package server

import (
	"crypto/subtle"
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
	if subtle.ConstantTimeCompare([]byte(hello.Token), []byte(h.server.cfg.Control.Token)) != 1 {
		h.reject(conn, control.CodeUnauthorized)
		return
	}
	if hello.ClientID == "" {
		h.reject(conn, control.CodeBadMessage)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	sess := &clientSession{
		id:       hello.ClientID,
		name:     hello.Name,
		version:  hello.Version,
		revision: hello.Revision,
		conn:     conn,
	}
	results := h.server.mappings.register(sess, hello.Mappings)

	ack := &control.HelloAck{
		Type:          control.TypeHelloAck,
		OK:            true,
		ServerVersion: version.Version,
		Results:       results,
	}
	if err := sess.write(ack); err != nil {
		h.server.mappings.detach(sess)
		return
	}

	h.log.Info("hy2tun client registered",
		"client", hello.ClientID,
		"name", hello.Name,
		"mappings", len(hello.Mappings),
	)
	h.loop(sess)
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
			results, accepted := h.server.mappings.update(sess, message.Revision, message.Mappings)
			if !accepted {
				h.log.Debug("rejected control update",
					"client", sess.id,
					"revision", message.Revision,
				)
			}
			if err := sess.write(&control.UpdateAck{
				Type:     control.TypeUpdateAck,
				Revision: h.server.mappings.sessionRevision(sess),
				Results:  results,
			}); err != nil {
				return
			}
		case *control.PushAck:
			h.log.Debug("received control push ack",
				"client", sess.id,
				"ok", message.OK,
				"revision", message.Revision,
			)
		case *control.Ping:
			if err := sess.write(&control.Pong{Type: control.TypePong, TS: message.TS}); err != nil {
				return
			}
		case *control.OpenResult:
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
