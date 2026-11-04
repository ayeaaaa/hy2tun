package server

import (
	"net"
	"time"

	"hy2tun/internal/transport"
	"hy2tun/internal/version"
)

// serveProbe owns only this independent child. It never sends ordinary Hello,
// allocates mappings, registers an ID, or touches the state store.
func (s *Server) serveProbe(conn net.Conn) {
	if conn == nil {
		return
	}
	defer conn.Close()
	select {
	case s.probeSlots <- struct{}{}:
		defer func() { <-s.probeSlots }()
	default:
		return
	}
	_ = conn.SetDeadline(time.Now().Add(controlHandshakeTimeout))
	req, err := transport.ReadProbeRequest(conn)
	if err != nil {
		_ = transport.WriteProbeReply(conn, transport.ProbeReply{Error: "bad_message"})
		return
	}
	if _, ok := s.currentCredentials().AcceptControlToken(req.Token); !ok {
		_ = transport.WriteProbeReply(conn, transport.ProbeReply{Error: "unauthorized"})
		return
	}
	_ = transport.WriteProbeReply(conn, transport.ProbeReply{OK: true, Info: transport.ProbeInfo{
		Version: 1, Nonce: req.Nonce, Mode: s.currentConfig().Mode.Effective(), ServerVersion: version.Version,
	}})
}
