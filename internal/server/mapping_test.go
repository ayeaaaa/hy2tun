package server

import (
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
)

type testRuntimeSnapshot struct {
	runtime      *mappingRuntime
	listener     net.Listener
	udpConn      *net.UDPConn
	udpToken     string
	status       string
	mapping      config.Mapping
	assignedPort int
}

func snapshotTestRuntime(t *testing.T, srv *Server, clientID, mappingID string) testRuntimeSnapshot {
	t.Helper()

	srv.mappings.mu.Lock()
	defer srv.mappings.mu.Unlock()

	sess := srv.mappings.sessions[clientID]
	require.NotNil(t, sess)
	rt := sess.mappings[mappingID]
	require.NotNil(t, rt)
	return testRuntimeSnapshot{
		runtime:      rt,
		listener:     rt.tcpListener,
		udpConn:      rt.udpConn,
		udpToken:     rt.udpToken,
		status:       rt.status,
		mapping:      rt.mapping,
		assignedPort: rt.assignedPort,
	}
}

func testSession(t *testing.T, srv *Server, clientID string) *clientSession {
	t.Helper()

	srv.mappings.mu.Lock()
	defer srv.mappings.mu.Unlock()
	sess := srv.mappings.sessions[clientID]
	require.NotNil(t, sess)
	return sess
}

func testSessionRevision(t *testing.T, srv *Server, clientID string) int64 {
	t.Helper()

	srv.mappings.mu.Lock()
	defer srv.mappings.mu.Unlock()
	sess := srv.mappings.sessions[clientID]
	require.NotNil(t, sess)
	return sess.revision
}

func testSessionAppliedRevision(t *testing.T, srv *Server, clientID string) int64 {
	t.Helper()

	srv.mappings.mu.Lock()
	defer srv.mappings.mu.Unlock()
	sess := srv.mappings.sessions[clientID]
	require.NotNil(t, sess)
	return sess.appliedRevision
}

func testSessionOrder(t *testing.T, srv *Server, clientID string) []string {
	t.Helper()

	srv.mappings.mu.Lock()
	defer srv.mappings.mu.Unlock()
	sess := srv.mappings.sessions[clientID]
	require.NotNil(t, sess)
	return append([]string(nil), sess.order...)
}

type trackingConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *trackingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func newTrackingConn() (*trackingConn, net.Conn) {
	local, remote := net.Pipe()
	return &trackingConn{Conn: local}, remote
}

func freeTCPPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}

func findResult(t *testing.T, results []control.Result, id string) control.Result {
	t.Helper()

	for _, result := range results {
		if result.ID == id {
			return result
		}
	}
	t.Fatalf("missing result for mapping %q", id)
	return control.Result{}
}

func TestUpdateKeepsUnchangedMappings(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		views := pair.Server.Clients()
		return len(views) == 1 && len(views[0].Mappings) == 2
	}))
	clientID := pair.Server.Clients()[0].ID
	sess := testSession(t, pair.Server, clientID)
	revision := testSessionRevision(t, pair.Server, clientID)
	beforeTCP := snapshotTestRuntime(t, pair.Server, clientID, "tcp1")
	beforeUDP := snapshotTestRuntime(t, pair.Server, clientID, "udp1")

	active, remote := newTrackingConn()
	defer remote.Close()
	defer active.Close()
	beforeTCP.runtime.trackActive(active)

	next := []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Note: "changed", Enabled: true},
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true},
	}
	results, accepted, _ := pair.Server.mappings.update(sess, revision+1, next)
	require.True(t, accepted)
	require.True(t, findResult(t, results, "tcp1").OK)
	require.True(t, findResult(t, results, "udp1").OK)
	require.False(t, active.closed.Load(), "note-only change must keep active connections")

	afterTCP := snapshotTestRuntime(t, pair.Server, clientID, "tcp1")
	afterUDP := snapshotTestRuntime(t, pair.Server, clientID, "udp1")
	require.Same(t, beforeTCP.runtime, afterTCP.runtime)
	require.Same(t, beforeUDP.runtime, afterUDP.runtime)
	require.Same(t, beforeTCP.listener, afterTCP.listener)
	require.Same(t, beforeUDP.udpConn, afterUDP.udpConn)
	require.Equal(t, beforeUDP.udpToken, afterUDP.udpToken)
	require.Equal(t, beforeTCP.assignedPort, afterTCP.assignedPort)
	require.Equal(t, beforeUDP.assignedPort, afterUDP.assignedPort)
	require.Equal(t, "changed", afterTCP.mapping.Note)
	require.Equal(t, "running", afterTCP.status)
	require.Equal(t, "running", afterUDP.status)
	require.Equal(t, revision+1, testSessionRevision(t, pair.Server, clientID))
}

func TestUpdateLocalAddrChangeKeepsRuntimeAndClosesActive(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		return len(pair.Server.Clients()) == 1
	}))
	clientID := pair.Server.Clients()[0].ID
	sess := testSession(t, pair.Server, clientID)
	revision := testSessionRevision(t, pair.Server, clientID)
	before := snapshotTestRuntime(t, pair.Server, clientID, "tcp1")

	active, remote := newTrackingConn()
	defer remote.Close()
	defer active.Close()
	before.runtime.trackActive(active)

	next := []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9999", Enabled: true},
	}
	results, accepted, _ := pair.Server.mappings.update(sess, revision+1, next)
	require.True(t, accepted)
	require.True(t, findResult(t, results, "tcp1").OK)
	require.True(t, active.closed.Load(), "local_addr change must close active connections")

	after := snapshotTestRuntime(t, pair.Server, clientID, "tcp1")
	require.Same(t, before.runtime, after.runtime)
	require.Same(t, before.listener, after.listener)
	require.Equal(t, before.assignedPort, after.assignedPort)
	require.Equal(t, "127.0.0.1:9999", after.mapping.LocalAddr)
	require.Equal(t, "running", after.status)
}

func TestUpdateRollsBackOnBadMapping(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		return len(pair.Server.Clients()) == 1
	}))
	clientID := pair.Server.Clients()[0].ID
	sess := testSession(t, pair.Server, clientID)
	revision := testSessionRevision(t, pair.Server, clientID)
	before := snapshotTestRuntime(t, pair.Server, clientID, "tcp1")

	validPort := freeTCPPort(t)
	occupied, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer occupied.Close()
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port

	next := []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
		{ID: "valid", Type: "tcp", RemotePort: validPort, LocalAddr: "127.0.0.1:9002", Enabled: true},
		{ID: "bad", Type: "tcp", RemotePort: occupiedPort, LocalAddr: "127.0.0.1:9003", Enabled: true},
	}
	results, accepted, _ := pair.Server.mappings.update(sess, revision+1, next)
	require.False(t, accepted)
	require.Equal(t, "internal", findResult(t, results, "valid").Error)
	require.Equal(t, "port_in_use", findResult(t, results, "bad").Error)

	after := snapshotTestRuntime(t, pair.Server, clientID, "tcp1")
	require.Same(t, before.runtime, after.runtime)
	require.Same(t, before.listener, after.listener)
	require.Equal(t, "running", after.status)
	require.Equal(t, revision, testSessionRevision(t, pair.Server, clientID))
	require.Equal(t, []string{"tcp1"}, testSessionOrder(t, pair.Server, clientID))

	// The pre-bound "valid" mapping must have been rolled back.
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", validPort))
	require.NoError(t, err)
	require.NoError(t, listener.Close())

	// The original listener is still serving.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", before.assignedPort))
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestUpdatePortChangeRebinds(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		return len(pair.Server.Clients()) == 1
	}))
	clientID := pair.Server.Clients()[0].ID
	sess := testSession(t, pair.Server, clientID)
	revision := testSessionRevision(t, pair.Server, clientID)
	before := snapshotTestRuntime(t, pair.Server, clientID, "tcp1")
	newPort := freeTCPPort(t)

	next := []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: newPort, LocalAddr: "127.0.0.1:9001", Enabled: true},
	}
	results, accepted, _ := pair.Server.mappings.update(sess, revision+1, next)
	require.True(t, accepted)
	require.Equal(t, newPort, findResult(t, results, "tcp1").AssignedPort)

	after := snapshotTestRuntime(t, pair.Server, clientID, "tcp1")
	require.NotSame(t, before.runtime, after.runtime)
	require.NotSame(t, before.listener, after.listener)
	require.Equal(t, newPort, after.assignedPort)
	require.Equal(t, "running", after.status)

	_, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", before.assignedPort), 200*time.Millisecond)
	require.Error(t, err, "old listener must be closed")

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", newPort), time.Second)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}
