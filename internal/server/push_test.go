package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"hy2tun/internal/client"
	"hy2tun/internal/config"
	"hy2tun/internal/control"
)

// waitForSettledClient waits until the first client has completed the
// hello/assigned-port write-back and returns its ID and revision. If a mapping
// failed to bind there is no write-back, so a terminal failed mapping also
// counts as settled.
func waitForSettledClient(t *testing.T, srv *Server) (string, int64) {
	t.Helper()

	var (
		id       string
		revision int64
	)
	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		views := srv.Clients()
		if len(views) != 1 {
			return false
		}
		if views[0].Revision > 0 {
			id, revision = views[0].ID, views[0].Revision
			return true
		}
		for _, mapping := range views[0].Mappings {
			if mapping.Status == "failed" {
				id, revision = views[0].ID, views[0].Revision
				return true
			}
		}
		return false
	}))
	return id, revision
}

func findPushResult(t *testing.T, results []control.Result, id string) control.Result {
	t.Helper()

	for _, result := range results {
		if result.ID == id {
			return result
		}
	}
	t.Fatalf("missing push result for mapping %q", id)
	return control.Result{}
}

func TestServerPushAppliesAndPersists(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	clientID, base := waitForSettledClient(t, pair.Server)
	require.Positive(t, base)
	before := pair.Server.Clients()[0]

	// The hello_ack write-back must already be persisted with the assigned
	// port and the bumped revision.
	savedBefore, err := config.LoadClient(pair.ClientConfigPath)
	require.NoError(t, err)
	require.Equal(t, base, savedBefore.Client.Revision)
	require.Equal(t, before.Mappings[0].AssignedPort, savedBefore.Mappings[0].RemotePort)

	next := []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Note: "changed", Enabled: true},
		{ID: "m2", Type: "udp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true},
	}
	rev, err := pair.Server.PushMappings(clientID, base, next)
	require.NoError(t, err)
	require.Equal(t, base+1, rev)

	saved, err := config.LoadClient(pair.ClientConfigPath)
	require.NoError(t, err)
	require.Equal(t, rev, saved.Client.Revision)
	require.Len(t, saved.Mappings, 2)
	require.Equal(t, "changed", saved.Mappings[0].Note)
	require.Equal(t, before.Mappings[0].AssignedPort, saved.Mappings[0].RemotePort)
	require.NotZero(t, saved.Mappings[1].RemotePort)

	views := pair.Server.Clients()
	require.Len(t, views, 1)
	require.Equal(t, rev, views[0].Revision)
	require.Len(t, views[0].Mappings, 2)
	require.Equal(t, before.Mappings[0].AssignedPort, views[0].Mappings[0].AssignedPort)
	require.Equal(t, "running", views[0].Mappings[1].Status)
	require.Equal(t, saved.Mappings[1].RemotePort, views[0].Mappings[1].AssignedPort)
}

func TestPushRejectsStaleBaseRevision(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	clientID, base := waitForSettledClient(t, pair.Server)

	_, err := pair.Server.PushMappings(clientID, base-1, []config.Mapping{
		{ID: "m2", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true},
	})
	require.ErrorIs(t, err, control.ErrConflict)

	saved, err := config.LoadClient(pair.ClientConfigPath)
	require.NoError(t, err)
	require.Len(t, saved.Mappings, 1)
	require.Equal(t, "m1", saved.Mappings[0].ID)
	require.Equal(t, base, saved.Client.Revision)

	_, err = pair.Server.PushMappings("missing", 0, nil)
	require.ErrorIs(t, err, control.ErrClientOffline)
}

func TestReconnectWithLowerRevisionIsAdopted(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	_, base := waitForSettledClient(t, pair.Server)
	require.Positive(t, base)

	require.NoError(t, pair.Client.Close())
	cfg, err := config.LoadClient(pair.ClientConfigPath)
	require.NoError(t, err)
	cfg.Client.Revision = 2
	data, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(pair.ClientConfigPath, data, 0600))

	cl2, err := client.New(cfg, slog.Default())
	require.NoError(t, err)
	defer cl2.Close()
	go func() { _ = cl2.Start(context.Background()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, cl2.WaitReady(ctx))

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		views := pair.Server.Clients()
		return len(views) == 1 && views[0].Revision == 2
	}))
}

func TestClientReconnectsWithCurrentRevision(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	_, base := waitForSettledClient(t, pair.Server)
	require.Positive(t, base)
	revision := pair.Client.Revision()
	require.Equal(t, base, revision)

	// Dropping every control session simulates a control/HY2 failure without
	// stopping the listener. The client must reconnect and hello with the
	// revision it already persisted.
	pair.Server.mappings.closeAll()

	require.NoError(t, waitFor(t, 8*time.Second, func() bool {
		views := pair.Server.Clients()
		return len(views) == 1 && views[0].Online && views[0].Revision == revision
	}))
	require.Equal(t, revision, pair.Client.Revision())
}

func TestPushRollbackOnBindFailure(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	clientID, base := waitForSettledClient(t, pair.Server)
	before := snapshotTestRuntime(t, pair.Server, clientID, "m1")

	occupied, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer occupied.Close()
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port
	validPort := freeTCPPort(t)

	next := []config.Mapping{
		before.mapping,
		{ID: "valid", Type: "tcp", RemotePort: validPort, LocalAddr: "127.0.0.1:9002", Enabled: true},
		{ID: "bad", Type: "tcp", RemotePort: occupiedPort, LocalAddr: "127.0.0.1:9003", Enabled: true},
	}
	_, err = pair.Server.PushMappings(clientID, base, next)
	require.Error(t, err)

	var pushErr *PushError
	require.ErrorAs(t, err, &pushErr)
	bad := findPushResult(t, pushErr.Results, "bad")
	require.Equal(t, "port_in_use", bad.Error)

	after := snapshotTestRuntime(t, pair.Server, clientID, "m1")
	require.Same(t, before.runtime, after.runtime)
	require.Same(t, before.listener, after.listener)
	require.Equal(t, before.assignedPort, after.assignedPort)
	require.Equal(t, base, testSessionRevision(t, pair.Server, clientID))

	// The valid mapping was pre-bound before the failure and must be released.
	released, err := net.Listen("tcp", fmt.Sprintf(":%d", validPort))
	require.NoError(t, err)
	require.NoError(t, released.Close())

	saved, err := config.LoadClient(pair.ClientConfigPath)
	require.NoError(t, err)
	require.Len(t, saved.Mappings, 1)
	require.Equal(t, "m1", saved.Mappings[0].ID)
	require.Equal(t, base, saved.Client.Revision)
}

func TestPushDeleteClosesActiveConnection(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	clientID, base := waitForSettledClient(t, pair.Server)
	before := snapshotTestRuntime(t, pair.Server, clientID, "m1")

	active, remote := newTrackingConn()
	defer remote.Close()
	before.runtime.trackActive(active)

	rev, err := pair.Server.PushMappings(clientID, base, nil)
	require.NoError(t, err)
	require.Equal(t, base+1, rev)
	require.True(t, active.closed.Load(), "delete must close active connections")
	require.True(t, before.runtime.closed.Load())

	views := pair.Server.Clients()
	require.Len(t, views, 1)
	require.Empty(t, views[0].Mappings)
	require.Equal(t, rev, views[0].Revision)

	saved, err := config.LoadClient(pair.ClientConfigPath)
	require.NoError(t, err)
	require.Empty(t, saved.Mappings)
	require.Equal(t, rev, saved.Client.Revision)

	require.Eventually(t, func() bool {
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", before.assignedPort))
		if err != nil {
			return false
		}
		_ = listener.Close()
		return true
	}, 2*time.Second, 20*time.Millisecond)
}

func TestPushNoteChangeKeepsListenerAndActive(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	clientID, base := waitForSettledClient(t, pair.Server)
	before := snapshotTestRuntime(t, pair.Server, clientID, "m1")

	active, remote := newTrackingConn()
	defer remote.Close()
	before.runtime.trackActive(active)

	next := []config.Mapping{before.mapping}
	next[0].Note = "note-only"
	rev, err := pair.Server.PushMappings(clientID, base, next)
	require.NoError(t, err)
	require.Equal(t, base+1, rev)
	require.False(t, active.closed.Load(), "note-only change must keep active connections")

	after := snapshotTestRuntime(t, pair.Server, clientID, "m1")
	require.Same(t, before.runtime, after.runtime)
	require.Same(t, before.listener, after.listener)
	require.Equal(t, before.assignedPort, after.assignedPort)
	require.Equal(t, "note-only", after.mapping.Note)
}

func TestPushLocalAddrChangeKeepsListenerAndClosesActive(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	clientID, base := waitForSettledClient(t, pair.Server)
	before := snapshotTestRuntime(t, pair.Server, clientID, "m1")

	active, remote := newTrackingConn()
	defer remote.Close()
	before.runtime.trackActive(active)

	next := []config.Mapping{before.mapping}
	next[0].LocalAddr = "127.0.0.1:9999"
	rev, err := pair.Server.PushMappings(clientID, base, next)
	require.NoError(t, err)
	require.Equal(t, base+1, rev)
	require.True(t, active.closed.Load(), "local_addr change must close active connections")

	after := snapshotTestRuntime(t, pair.Server, clientID, "m1")
	require.Same(t, before.runtime, after.runtime)
	require.Same(t, before.listener, after.listener)
	require.Equal(t, before.assignedPort, after.assignedPort)
	require.Equal(t, "127.0.0.1:9999", after.mapping.LocalAddr)
}

func TestUpdateAcceptsResyncRevisionAfterPushConflict(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	clientID, base := waitForSettledClient(t, pair.Server)
	sess := testSession(t, pair.Server, clientID)
	before := snapshotTestRuntime(t, pair.Server, clientID, "m1")

	// Simulate a push conflict ack that reported the client's newer revision
	// while the client's update is still queued.
	pair.Server.mappings.adoptClientRevision(sess, base+1)
	require.Equal(t, base+1, testSessionRevision(t, pair.Server, clientID))

	next := []config.Mapping{
		before.mapping,
		{ID: "m2", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true},
	}
	results, accepted := pair.Server.mappings.update(sess, base+1, next)
	require.True(t, accepted)
	require.True(t, findResult(t, results, "m2").OK)
	require.Equal(t, base+1, testSessionRevision(t, pair.Server, clientID))

	// The resync allowance is one-shot: the same revision is stale again.
	results, accepted = pair.Server.mappings.update(sess, base+1, next)
	require.False(t, accepted)
	require.Equal(t, "stale_revision", results[0].Error)
}

func TestClientDeleteMappingClosesActiveConnection(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	clientID, base := waitForSettledClient(t, pair.Server)
	before := snapshotTestRuntime(t, pair.Server, clientID, "m1")

	active, remote := newTrackingConn()
	defer remote.Close()
	before.runtime.trackActive(active)

	require.NoError(t, pair.Client.DeleteMapping("m1"))
	require.Equal(t, base+1, pair.Client.Revision())

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		views := pair.Server.Clients()
		return len(views) == 1 && views[0].Revision == base+1 && len(views[0].Mappings) == 0
	}))
	require.True(t, active.closed.Load(), "local delete must close active connections")
	require.True(t, before.runtime.closed.Load())

	saved, err := config.LoadClient(pair.ClientConfigPath)
	require.NoError(t, err)
	require.Empty(t, saved.Mappings)
	require.Equal(t, base+1, saved.Client.Revision)
}

func TestClientLocalEditUpdatesServerAndWritesAssignedPort(t *testing.T) {
	pair := startTestPair(t, []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
	})
	defer pair.Stop()

	_, base := waitForSettledClient(t, pair.Server)
	require.Positive(t, base)

	require.NoError(t, pair.Client.AddMapping(config.Mapping{
		ID: "m2", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true,
	}))
	require.Equal(t, base+1, pair.Client.Revision())

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		views := pair.Server.Clients()
		if len(views) != 1 || views[0].Revision != base+1 || len(views[0].Mappings) != 2 {
			return false
		}
		saved, err := config.LoadClient(pair.ClientConfigPath)
		if err != nil {
			return false
		}
		return saved.Client.Revision == base+1 &&
			len(saved.Mappings) == 2 &&
			saved.Mappings[1].RemotePort != 0
	}))

	saved, err := config.LoadClient(pair.ClientConfigPath)
	require.NoError(t, err)
	require.Equal(t, base+1, saved.Client.Revision)
	require.Equal(t, pair.Client.Revision(), saved.Client.Revision)

	views := pair.Server.Clients()
	require.Equal(t, views[0].Mappings[1].AssignedPort, saved.Mappings[1].RemotePort)
	require.Equal(t, "running", views[0].Mappings[1].Status)
}
