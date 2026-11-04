package transport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Removing the prefix check would read/allocate an attacker-selected body.
func TestTCPWireBoundedJSON(t *testing.T) {
	r := &prefixOnlyReader{prefix: []byte{0x40, 0, 0, 0}}
	_, err := ReadProbeRequest(r)
	require.ErrorIs(t, err, ErrProtocol)
	require.False(t, r.bodyRead)
	for _, body := range []string{`null`, `{}`, `{"version":1,"token":"secret","nonce":"bad"}`, `{"version":1,"token":"secret","nonce":"0123456789abcdef0123456789abcdef"} {}`} {
		var b bytes.Buffer
		require.NoError(t, binary.Write(&b, binary.BigEndian, uint32(len(body))))
		b.WriteString(body)
		_, err := ReadProbeRequest(&b)
		require.ErrorIs(t, err, ErrProtocol)
	}
	var b bytes.Buffer
	req := ProbeRequest{Version: 1, Token: "secret", Nonce: "0123456789abcdef0123456789abcdef"}
	require.NoError(t, WriteProbeRequest(&b, req))
	got, err := ReadProbeRequest(&oneByteReader{r: &b})
	require.NoError(t, err)
	require.Equal(t, req, got)
	require.ErrorIs(t, WriteProbeRequest(io.Discard, ProbeRequest{Version: 1, Token: strings.Repeat("x", 65537), Nonce: req.Nonce}), ErrProtocol)
}

func TestTCPProbeReplySnakeCase(t *testing.T) {
	var b bytes.Buffer
	require.NoError(t, WriteProbeReply(&b, ProbeReply{OK: true, Info: ProbeInfo{Version: 1, Nonce: "0123456789abcdef0123456789abcdef", Mode: "hy2+tcp", ServerVersion: "test"}}))
	var reply map[string]any
	require.NoError(t, json.Unmarshal(b.Bytes()[4:], &reply))
	info := reply["info"].(map[string]any)
	require.Equal(t, map[string]any{"version": float64(1), "nonce": "0123456789abcdef0123456789abcdef", "mode": "hy2+tcp", "server_version": "test"}, info)
}

type oneByteReader struct{ r io.Reader }

func (r *oneByteReader) Read(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return r.r.Read(b)
}

type prefixOnlyReader struct {
	prefix   []byte
	bodyRead bool
}

func (r *prefixOnlyReader) Read(b []byte) (int, error) {
	if len(r.prefix) == 0 {
		r.bodyRead = true
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(b, r.prefix)
	r.prefix = r.prefix[n:]
	return n, nil
}
