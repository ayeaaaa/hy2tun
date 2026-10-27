package server

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseVirtualAddr(t *testing.T) {
	kind, token, ok := parseVirtualAddr("_hy2tun_control_:0")
	require.True(t, ok)
	require.Equal(t, "control", kind)
	require.Empty(t, token)

	kind, token, ok = parseVirtualAddr("_hy2tun_data_abc123:0")
	require.True(t, ok)
	require.Equal(t, "data", kind)
	require.Equal(t, "abc123", token)

	kind, token, ok = parseVirtualAddr("_hy2tun_udp_def456:0")
	require.True(t, ok)
	require.Equal(t, "udp", kind)
	require.Equal(t, "def456", token)

	_, _, ok = parseVirtualAddr("example.com:443")
	require.False(t, ok)
}
