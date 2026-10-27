package server

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/testutil"
)

func TestReverseTCPEcho(t *testing.T) {
	echoAddr, stopEcho := testutil.StartTCPEcho(t)
	defer stopEcho()

	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1 && v[0].Mappings[0].Status == "running"
	}))
	port := pair.Server.Clients()[0].Mappings[0].AssignedPort

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf))
}

func TestDeletingMappingClosesActiveConnection(t *testing.T) {
	echoAddr, stopEcho := testutil.StartTCPEcho(t)
	defer stopEcho()
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1
	}))
	port := pair.Server.Clients()[0].Mappings[0].AssignedPort
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	defer conn.Close()

	// Prove the reverse stream is attached before deleting the mapping.
	requireTCPEcho(t, conn, "before")

	require.NoError(t, pair.Server.removeMapping(pair.Server.Clients()[0].ID, "tcp1"))
	require.Empty(t, pair.Server.Clients()[0].Mappings)
	require.NoError(t, waitFor(t, 3*time.Second, func() bool {
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, err := conn.Read(make([]byte, 1))
		return err != nil && !isTimeout(err)
	}), "active connection must be closed, not merely idle")

	// The listener is gone and the port can be bound again.
	require.NoError(t, waitFor(t, 3*time.Second, func() bool {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return false
		}
		return listener.Close() == nil
	}), "removed mapping must release its public port")
}

func TestReverseTCPDialFailureClosesPublicConnection(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t)), Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		v := pair.Server.Clients()
		return len(v) == 1 && len(v[0].Mappings) == 1 && v[0].Mappings[0].Status == "running"
	}))
	port := pair.Server.Clients()[0].Mappings[0].AssignedPort

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err)
	defer conn.Close()

	// The client reports dial_failed, so the public peer must be released
	// without waiting for the 10-second token TTL.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, err = conn.Read(make([]byte, 1))
	require.Error(t, err)
	require.False(t, isTimeout(err), "public connection must be closed after a dial failure")
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func TestDataStreamTokenSingleUseAndExpiry(t *testing.T) {
	data := newDataStreams()
	defer data.closeAll()

	open, peer := net.Pipe()
	defer peer.Close()
	runtime := &mappingRuntime{}
	token, err := data.add(&pendingConn{
		conn:    open,
		mapping: runtime,
		expires: time.Now().Add(time.Minute),
	})
	require.NoError(t, err)
	require.Len(t, token, 32, "a data token must be 128 bits of hex")
	_, err = hex.DecodeString(token)
	require.NoError(t, err)

	conn, err := data.take(token)
	require.NoError(t, err)
	require.Same(t, open, conn)
	_, err = data.take(token)
	require.Error(t, err, "a token must only be claimed once")

	expiredOpen, expiredPeer := net.Pipe()
	defer expiredPeer.Close()
	expired, err := data.add(&pendingConn{
		conn:    expiredOpen,
		mapping: runtime,
		expires: time.Now().Add(-time.Second),
	})
	require.NoError(t, err)
	_, err = data.take(expired)
	require.Error(t, err, "an expired token must not be claimable")
}
