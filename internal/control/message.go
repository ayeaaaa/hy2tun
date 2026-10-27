// Package control implements the framed JSON control protocol spoken between
// the hy2tun client and server over the HY2 control stream.
package control

import "hy2tun/internal/config"

// Message type names carried in the "type" field of every control message.
const (
	TypeHello      = "hello"
	TypeHelloAck   = "hello_ack"
	TypeUpdate     = "update"
	TypeUpdateAck  = "update_ack"
	TypePush       = "push"
	TypePushAck    = "push_ack"
	TypeOpen       = "open"
	TypeOpenResult = "open_result"
	TypePing       = "ping"
	TypePong       = "pong"
)

// Hello is sent by the client immediately after opening the control stream.
// It authenticates the client and publishes the authoritative mapping list.
type Hello struct {
	Type     string           `json:"type"`
	Protocol int              `json:"protocol"`
	Token    string           `json:"token"`
	ClientID string           `json:"client_id"`
	Name     string           `json:"name"`
	Version  string           `json:"version"`
	Revision int64            `json:"revision"`
	Mappings []config.Mapping `json:"mappings"`
}

// HelloAck is the server's reply to Hello. A false OK value means the client
// was rejected; Error carries the stable error code.
type HelloAck struct {
	Type          string   `json:"type"`
	OK            bool     `json:"ok"`
	ServerVersion string   `json:"server_version"`
	Results       []Result `json:"results"`
	Error         string   `json:"error"`
}

// Result reports the outcome of one mapping operation.
type Result struct {
	ID           string `json:"id"`
	OK           bool   `json:"ok"`
	AssignedPort int    `json:"assigned_port"`
	UDPToken     string `json:"udp_token"`
	Error        string `json:"error"`
}

// Update is sent by the client after a local configuration change. It carries
// the complete mapping list and the client's new revision.
type Update struct {
	Type     string           `json:"type"`
	Revision int64            `json:"revision"`
	Mappings []config.Mapping `json:"mappings"`
}

// UpdateAck is the server's reply to Update.
type UpdateAck struct {
	Type     string   `json:"type"`
	Revision int64    `json:"revision"`
	Results  []Result `json:"results"`
}

// Push is sent by the server to apply an authoritative mapping list. The
// client must still hold BaseRevision for the push to succeed. Results carries
// the server's per-mapping outcome, including the UDP token for reverse UDP
// mappings that were pre-bound for this push. ID correlates the ack with this
// specific push, so a late ack from a timed-out push cannot be mistaken for a
// retry's ack.
type Push struct {
	Type         string           `json:"type"`
	ID           string           `json:"id,omitempty"`
	BaseRevision int64            `json:"base_revision"`
	Revision     int64            `json:"revision"`
	Mappings     []config.Mapping `json:"mappings"`
	Results      []Result         `json:"results,omitempty"`
}

// PushAck is the client's reply to Push. On conflict OK is false, Error is
// "conflict", and Revision is the client's current revision. ID echoes the
// Push.ID it acknowledges.
type PushAck struct {
	Type     string   `json:"type"`
	ID       string   `json:"id,omitempty"`
	OK       bool     `json:"ok"`
	Revision int64    `json:"revision"`
	Results  []Result `json:"results"`
	Error    string   `json:"error"`
}

// Open asks the client to dial a local target for an incoming public
// connection identified by Token.
type Open struct {
	Type      string `json:"type"`
	Token     string `json:"token"`
	MappingID string `json:"mapping_id"`
}

// OpenResult optionally reports the outcome of an Open to the server.
type OpenResult struct {
	Type  string `json:"type"`
	Token string `json:"token"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// Ping is a control-stream keepalive. TS is a Unix timestamp in seconds.
type Ping struct {
	Type string `json:"type"`
	TS   int64  `json:"ts"`
}

// Pong replies to Ping and echoes its timestamp.
type Pong struct {
	Type string `json:"type"`
	TS   int64  `json:"ts"`
}

// newMessage returns an empty message value for a wire type name.
func newMessage(typ string) (any, bool) {
	switch typ {
	case TypeHello:
		return &Hello{}, true
	case TypeHelloAck:
		return &HelloAck{}, true
	case TypeUpdate:
		return &Update{}, true
	case TypeUpdateAck:
		return &UpdateAck{}, true
	case TypePush:
		return &Push{}, true
	case TypePushAck:
		return &PushAck{}, true
	case TypeOpen:
		return &Open{}, true
	case TypeOpenResult:
		return &OpenResult{}, true
	case TypePing:
		return &Ping{}, true
	case TypePong:
		return &Pong{}, true
	default:
		return nil, false
	}
}
