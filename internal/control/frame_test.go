package control

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// writeRawFrame writes an arbitrary payload with a valid length prefix. It is
// used to feed hand-crafted (including hostile) frames to ReadMessage.
func writeRawFrame(t *testing.T, buf *bytes.Buffer, payload []byte) {
	t.Helper()
	require.NoError(t, binary.Write(buf, binary.BigEndian, uint32(len(payload))))
	_, err := buf.Write(payload)
	require.NoError(t, err)
}

func TestReadMessageRejectsOversized(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint32(MaxFrameSize+1)))
	_, err := ReadMessage(&buf)
	require.ErrorIs(t, err, ErrFrameTooLarge)
}

func TestReadMessageRejectsTruncated(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint32(10)))
	buf.WriteString(`{"type":`)
	_, err := ReadMessage(&buf)
	require.Error(t, err)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestReadMessageRejectsTruncatedHeader(t *testing.T) {
	buf := bytes.NewBuffer([]byte{0x00, 0x01})
	_, err := ReadMessage(buf)
	require.Error(t, err)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestReadMessageRejectsUnknownType(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteMessage(&buf, map[string]any{"type": "nope"}))
	_, err := ReadMessage(&buf)
	require.ErrorIs(t, err, ErrUnknownType)
}

func TestReadMessageRejectsMissingType(t *testing.T) {
	var buf bytes.Buffer
	writeRawFrame(t, &buf, []byte(`{}`))
	_, err := ReadMessage(&buf)
	require.ErrorIs(t, err, ErrUnknownType)
}

func TestReadMessageDecodesCredentialMessages(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    any
	}{
		{
			name:    "credential_update",
			payload: `{"type":"credential_update","generation":2,"auth":"a","control_token":"t"}`,
			want:    &CredentialUpdate{Type: TypeCredentialUpdate, Generation: 2, Auth: "a", ControlToken: "t"},
		},
		{
			name:    "credential_ack",
			payload: `{"type":"credential_ack","generation":2,"ok":true,"error":"stale_generation"}`,
			want:    &CredentialAck{Type: TypeCredentialAck, Generation: 2, OK: true, Error: "stale_generation"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			writeRawFrame(t, &buf, []byte(tt.payload))
			out, err := ReadMessage(&buf)
			require.NoError(t, err)
			require.Equal(t, tt.want, out)
		})
	}
}

func TestReadMessageAcceptsLegacyHelloWithoutCredentialFields(t *testing.T) {
	var buf bytes.Buffer
	writeRawFrame(t, &buf, []byte(`{"type":"hello","protocol":1,"token":"token","client_id":"c1"}`))
	out, err := ReadMessage(&buf)
	require.NoError(t, err)
	require.Equal(t, &Hello{Type: TypeHello, Protocol: 1, Token: "token", ClientID: "c1"}, out)
}

func TestNewControlFieldsUseWireNames(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    any
	}{
		{
			name:    "hello",
			payload: `{"type":"hello","protocol":1,"credential_generation":2,"capabilities":["credential_update_v1"]}`,
			want: &Hello{
				Type: TypeHello, Protocol: 1, CredentialGeneration: 2,
				Capabilities: []string{CapabilityCredentialUpdate},
			},
		},
		{
			name:    "hello_ack",
			payload: `{"type":"hello_ack","ok":true,"credential_generation":2,"credential_synced":true}`,
			want: &HelloAck{
				Type: TypeHelloAck, OK: true, CredentialGeneration: 2, CredentialSynced: true,
			},
		},
		{
			name:    "update",
			payload: `{"type":"update","revision":3,"name":"new-name"}`,
			want:    &Update{Type: TypeUpdate, Revision: 3, Name: "new-name"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			writeRawFrame(t, &buf, []byte(tt.payload))
			out, err := ReadMessage(&buf)
			require.NoError(t, err)
			require.Equal(t, tt.want, out)
		})
	}
}

func TestUpdateOmitsEmptyName(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteMessage(&buf, &Update{Type: TypeUpdate, Revision: 3}))
	require.NotContains(t, buf.String(), `"name"`)
}

func TestReadMessageRejectsZeroLength(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, binary.Write(&buf, binary.BigEndian, uint32(0)))
	_, err := ReadMessage(&buf)
	require.ErrorIs(t, err, ErrBadMessage)
}

func TestReadMessageRejectsMalformedJSON(t *testing.T) {
	tests := map[string]string{
		"truncated json":  `{"type":`,
		"not an object":   `[]`,
		"non-string type": `{"type":123}`,
		"bad field type":  `{"type":"ping","ts":"soon"}`,
		"garbage":         "\xff\xfe",
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			writeRawFrame(t, &buf, []byte(payload))
			_, err := ReadMessage(&buf)
			require.ErrorIs(t, err, ErrBadMessage)
		})
	}
}

func TestWriteMessageRejectsOversized(t *testing.T) {
	in := &OpenResult{Type: TypeOpenResult, Token: strings.Repeat("x", MaxFrameSize), OK: true}
	var buf bytes.Buffer
	err := WriteMessage(&buf, in)
	require.ErrorIs(t, err, ErrFrameTooLarge)
	require.Zero(t, buf.Len(), "no bytes must be written for a rejected frame")
}

func TestWriteMessageRejectsNil(t *testing.T) {
	var buf bytes.Buffer
	require.ErrorIs(t, WriteMessage(&buf, nil), ErrBadMessage)
	require.Zero(t, buf.Len())
}

func TestReadMessageAcceptsMaxFrameSize(t *testing.T) {
	base, err := json.Marshal(&OpenResult{Type: TypeOpenResult, OK: true})
	require.NoError(t, err)
	in := &OpenResult{Type: TypeOpenResult, Token: strings.Repeat("x", MaxFrameSize-len(base)), OK: true}
	payload, err := json.Marshal(in)
	require.NoError(t, err)
	require.Len(t, payload, MaxFrameSize)

	var buf bytes.Buffer
	require.NoError(t, WriteMessage(&buf, in))
	out, err := ReadMessage(&buf)
	require.NoError(t, err)
	require.Equal(t, in, out)
}

func TestReadMessageMultipleFrames(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteMessage(&buf, &Ping{Type: TypePing, TS: 1}))
	require.NoError(t, WriteMessage(&buf, &Pong{Type: TypePong, TS: 1}))

	first, err := ReadMessage(&buf)
	require.NoError(t, err)
	require.Equal(t, &Ping{Type: TypePing, TS: 1}, first)

	second, err := ReadMessage(&buf)
	require.NoError(t, err)
	require.Equal(t, &Pong{Type: TypePong, TS: 1}, second)

	_, err = ReadMessage(&buf)
	require.ErrorIs(t, err, io.EOF)
}

// chunkWriter accepts at most one byte per call to exercise short writes.
type chunkWriter struct {
	buf bytes.Buffer
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return w.buf.Write(p[:1])
}

func TestWriteMessageHandlesShortWrites(t *testing.T) {
	w := &chunkWriter{}
	in := &Hello{Type: TypeHello, Protocol: ProtocolVersion, Token: "secret", ClientID: "c1", Mappings: nil}
	require.NoError(t, WriteMessage(w, in))

	out, err := ReadMessage(&w.buf)
	require.NoError(t, err)
	require.Equal(t, in, out)
}

// stalledWriter reports success without consuming any bytes.
type stalledWriter struct{}

func (stalledWriter) Write([]byte) (int, error) { return 0, nil }

func TestWriteMessageRejectsStalledWriter(t *testing.T) {
	err := WriteMessage(stalledWriter{}, &Ping{Type: TypePing, TS: 1})
	require.ErrorIs(t, err, io.ErrShortWrite)
}

func FuzzReadMessage(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{0x00, 0x00, 0x00, 0x02, '{', '}'})
	f.Add([]byte{0x00, 0x00, 0x00, 0x08, '{', '"', 't', 'y', 'p', 'e', '"', ':'})
	f.Add([]byte{0x00, 0x00, 0x00, 0x10, '{', '"', 't', 'y', 'p', 'e', '"', ':', '"', 'p', 'i', 'n', 'g', '"', '}'})
	f.Fuzz(func(t *testing.T, data []byte) {
		// Hostile input must return an error, never panic or block.
		_, _ = ReadMessage(bytes.NewReader(data))
	})
}

func TestFrameErrorsAreDistinct(t *testing.T) {
	all := []error{ErrFrameTooLarge, ErrBadMessage, ErrUnknownType, ErrConflict, ErrClientOffline, ErrPushTimeout}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			require.False(t, errors.Is(a, b), "%v must not match %v", a, b)
		}
	}
}
