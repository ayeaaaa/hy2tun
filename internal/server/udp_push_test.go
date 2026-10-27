package server

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/testutil"
)

// TestPendingUDPTokenVisibleBeforeCommit proves that a push pre-binds a UDP
// mapping and publishes its token before the transaction commits, so a client
// probe sent right after push_ack cannot miss the runtime.
func TestPendingUDPTokenVisibleBeforeCommit(t *testing.T) {
	echoAddr, stopEcho := testutil.StartUDPEcho(t)
	defer stopEcho()

	srv, stop := startTestServer(t)
	defer stop()

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	sess := &clientSession{
		id:              "pending-udp",
		online:          true,
		revision:        1,
		appliedRevision: 1,
		conn:            serverConn,
		mappings:        make(map[string]*mappingRuntime),
	}
	srv.mappings.register(sess, nil)

	srv.mappings.mu.Lock()
	plans, results, err := srv.mappings.preparePushLocked(sess, []config.Mapping{
		{ID: "u1", Type: "udp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	})
	srv.mappings.mu.Unlock()
	require.NoError(t, err)
	require.Len(t, plans, 1)
	require.Len(t, results, 1)
	require.True(t, results[0].OK)
	require.NotEmpty(t, results[0].UDPToken)

	runtime, err := srv.mappings.udpRuntime(results[0].UDPToken)
	require.NoError(t, err, "a pre-bound push runtime must be visible before commit")
	require.Same(t, plans[0].fresh, runtime)

	// Rollback removes the token before closing the socket.
	srv.mappings.mu.Lock()
	for _, plan := range plans {
		srv.mappings.dropPendingUDPLocked(plan.fresh)
	}
	srv.mappings.mu.Unlock()
	for _, plan := range plans {
		plan.fresh.close()
	}
	_, err = srv.mappings.udpRuntime(results[0].UDPToken)
	require.Error(t, err, "a rolled-back token must not stay visible")
}

// TestPushUDPDatagramImmediatelyAfterAck pushes a new UDP mapping and sends a
// datagram as soon as the push returns. The client's probe and the public
// datagram may race the commit; the mapping must still reply immediately
// instead of staying deaf until a later keepalive or reconnect.
func TestPushUDPDatagramImmediatelyAfterAck(t *testing.T) {
	echoAddr, stopEcho := testutil.StartUDPEcho(t)
	defer stopEcho()

	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	clientID, base := waitForSettledClient(t, pair.Server)

	next := []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: echoAddr, Enabled: true},
	}
	revision, err := pair.Server.PushMappings(clientID, base, next)
	require.NoError(t, err)
	require.Equal(t, base+1, revision)

	var port int
	for _, mapping := range pair.Server.Clients()[0].Mappings {
		if mapping.ID == "udp1" {
			port = mapping.AssignedPort
		}
	}
	require.NotZero(t, port)

	// No retry loop: the first datagram after the ack must be answered.
	reply, err := exchangeUDP(fmt.Sprintf("127.0.0.1:%d", port), []byte("push-udp"), 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, "push-udp", string(reply))
}
