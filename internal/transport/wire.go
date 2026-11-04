package transport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"hy2tun/internal/config"
)

const (
	greetingLimit    = 65536
	streamFrameLimit = 4096
	protocolVersion  = 1
	protocolMagic    = "hy2tun-tcp"
)

type Greeting struct {
	Magic        string `json:"magic"`
	Version      int    `json:"version"`
	Auth         string `json:"auth"`
	ControlToken string `json:"control_token"`
	ClientID     string `json:"client_id"`
	Up           uint64 `json:"up"`
	Down         uint64 `json:"down"`
}

type GreetingAck struct {
	OK            bool                 `json:"ok"`
	Version       int                  `json:"version"`
	Mode          config.TransportMode `json:"mode"`
	ServerVersion string               `json:"server_version"`
	Error         string               `json:"error,omitempty"`
}

type ProbeRequest struct {
	Version int    `json:"version"`
	Token   string `json:"token"`
	Nonce   string `json:"nonce"`
}

type ProbeReply struct {
	OK    bool      `json:"ok"`
	Error string    `json:"error,omitempty"`
	Info  ProbeInfo `json:"info"`
}

func WriteProbeRequest(w io.Writer, req ProbeRequest) error {
	if !validProbeRequest(req) {
		return ErrProtocol
	}
	return writeJSONFrame(w, req, greetingLimit)
}

func ReadProbeRequest(r io.Reader) (ProbeRequest, error) {
	var req ProbeRequest
	if err := readJSONFrame(r, &req, greetingLimit); err != nil {
		return req, err
	}
	if !validProbeRequest(req) {
		return ProbeRequest{}, ErrProtocol
	}
	return req, nil
}

func validProbeRequest(req ProbeRequest) bool {
	return req.Version == protocolVersion && len(req.Token) > 0 && len(req.Token) <= greetingLimit && validHY2Token(req.Nonce)
}

func WriteProbeReply(w io.Writer, reply ProbeReply) error {
	if !validProbeReply(reply) {
		return ErrProtocol
	}
	return writeJSONFrame(w, reply, greetingLimit)
}

func ReadProbeReply(r io.Reader) (ProbeReply, error) {
	var reply ProbeReply
	if err := readJSONFrame(r, &reply, greetingLimit); err != nil {
		return reply, err
	}
	if !validProbeReply(reply) {
		return ProbeReply{}, ErrProtocol
	}
	return reply, nil
}

func validProbeReply(reply ProbeReply) bool {
	if !reply.OK {
		return safeWireError(reply.Error) && reply.Info == (ProbeInfo{})
	}
	return reply.Error == "" && reply.Info.Version == protocolVersion && validHY2Token(reply.Info.Nonce) && validWireMode(reply.Info.Mode) && len(reply.Info.ServerVersion) <= greetingLimit
}

func validWireMode(mode config.TransportMode) bool { return mode != "" && mode.Validate() == nil }
func safeWireError(s string) bool {
	return s == "unauthorized" || s == "unsupported_protocol" || s == "bad_message"
}
func wireError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrAuthentication) {
		return "unauthorized"
	}
	return "bad_message"
}

// The length prefix is checked before allocating or touching the body. JSON
// syntax/shape errors are deliberately not returned with credential-bearing text.
func readJSONFrame(r io.Reader, value any, limit uint32) error {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n == 0 || n > limit {
		return ErrProtocol
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(buf), []byte("null")) {
		return ErrProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(buf))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return ErrProtocol
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ErrProtocol
	}
	return nil
}

func writeJSONFrame(w io.Writer, value any, limit uint32) error {
	body, err := json.Marshal(value)
	if err != nil || len(body) == 0 || uint64(len(body)) > uint64(limit) {
		return ErrProtocol
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(body)))
	if err := writeFull(w, prefix[:]); err != nil {
		return err
	}
	return writeFull(w, body)
}

func writeFull(w io.Writer, buf []byte) error {
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if n < 0 || n > len(buf) {
			return io.ErrShortWrite
		}
		buf = buf[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
