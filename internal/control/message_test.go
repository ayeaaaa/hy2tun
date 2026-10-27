package control

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
)

func TestControlRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := &Hello{
		Type: "hello", Protocol: 1, Token: "token", ClientID: "c1",
		Name: "home", Version: "0.1.0", Revision: 3,
		Mappings: []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222, LocalAddr: "127.0.0.1:22", Note: "ssh", Enabled: true}},
	}
	require.NoError(t, WriteMessage(&buf, in))
	out, err := ReadMessage(&buf)
	require.NoError(t, err)
	require.Equal(t, in, out)
}

func TestControlRoundTripAllMessageTypes(t *testing.T) {
	mappings := []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 2222, LocalAddr: "127.0.0.1:22", Note: "ssh", Enabled: true},
		{ID: "m2", Type: "udp", RemotePort: 0, LocalAddr: "127.0.0.1:53", Note: "dns", Enabled: false},
	}
	results := []Result{
		{ID: "m1", OK: true, AssignedPort: 2222},
		{ID: "m2", OK: false, Error: string(CodePortInUse)},
	}
	tests := []struct {
		name string
		msg  any
	}{
		{"hello", &Hello{Type: TypeHello, Protocol: ProtocolVersion, Token: "token", ClientID: "c1", Name: "home", Version: "0.1.0", Revision: 7, Mappings: mappings}},
		{"hello_ack", &HelloAck{Type: TypeHelloAck, OK: true, ServerVersion: "0.1.0", Results: results}},
		{"hello_ack error", &HelloAck{Type: TypeHelloAck, OK: false, Error: string(CodeUnauthorized)}},
		{"update", &Update{Type: TypeUpdate, Revision: 8, Mappings: mappings}},
		{"update_ack", &UpdateAck{Type: TypeUpdateAck, Revision: 8, Results: results}},
		{"push", &Push{Type: TypePush, BaseRevision: 8, Revision: 9, Mappings: mappings}},
		{"push_ack", &PushAck{Type: TypePushAck, OK: true, Revision: 9, Results: results}},
		{"push_ack conflict", &PushAck{Type: TypePushAck, OK: false, Revision: 8, Error: string(CodeConflict)}},
		{"open", &Open{Type: TypeOpen, Token: "a1b2", MappingID: "m1"}},
		{"open_result", &OpenResult{Type: TypeOpenResult, Token: "a1b2", OK: false, Error: "dial_failed"}},
		{"ping", &Ping{Type: TypePing, TS: 1793076000}},
		{"pong", &Pong{Type: TypePong, TS: 1793076000}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, WriteMessage(&buf, tt.msg))
			out, err := ReadMessage(&buf)
			require.NoError(t, err)
			require.Equal(t, tt.msg, out)
		})
	}
}

func TestReadMessageIgnoresUnknownFields(t *testing.T) {
	var buf bytes.Buffer
	writeRawFrame(t, &buf, []byte(`{"type":"ping","ts":42,"future":{"nested":true}}`))
	out, err := ReadMessage(&buf)
	require.NoError(t, err)
	require.Equal(t, &Ping{Type: TypePing, TS: 42}, out)
}

func TestErrorCodeValues(t *testing.T) {
	// These strings are part of the wire contract (spec §6.4).
	want := map[ErrorCode]string{
		CodeUnauthorized:        "unauthorized",
		CodeUnsupportedProtocol: "unsupported_protocol",
		CodeBadMessage:          "bad_message",
		CodePortNotAllowed:      "port_not_allowed",
		CodePortReserved:        "port_reserved",
		CodePortInUse:           "port_in_use",
		CodeNoFreePort:          "no_free_port",
		CodeInvalidTarget:       "invalid_target",
		CodeConflict:            "conflict",
		CodeStaleRevision:       "stale_revision",
		CodeInternal:            "internal",
	}
	require.Len(t, want, 11)
	for code, value := range want {
		require.Equal(t, value, string(code))
	}
}

func TestMessageTypeNames(t *testing.T) {
	types := []string{
		TypeHello, TypeHelloAck, TypeUpdate, TypeUpdateAck, TypePush,
		TypePushAck, TypeOpen, TypeOpenResult, TypePing, TypePong,
	}
	seen := make(map[string]bool, len(types))
	for _, typ := range types {
		require.NotEmpty(t, typ)
		require.False(t, seen[typ], "duplicate message type %q", typ)
		seen[typ] = true
		_, ok := newMessage(typ)
		require.True(t, ok, "type %q must have a message constructor", typ)
	}
}
