package server

import (
	"fmt"
	"net"
	"testing"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
)

// openRawControl opens a control stream with the official HY2 client. It lets
// tests drive the control protocol directly, including dropping an ack.
func openRawControl(t *testing.T, hy hyclient.Client) net.Conn {
	t.Helper()

	conn, err := hy.TCP("_hy2tun_control_:0")
	require.NoError(t, err)
	require.NotNil(t, conn)
	return conn
}

// rawHello sends Hello and returns the server's HelloAck.
func rawHello(t *testing.T, conn net.Conn, clientID string, revision int64, mappings []config.Mapping) *control.HelloAck {
	t.Helper()

	require.NoError(t, control.WriteMessage(conn, &control.Hello{
		Type:     control.TypeHello,
		Protocol: control.ProtocolVersion,
		Token:    "0123456789abcdef0123456789abcdef",
		ClientID: clientID,
		Name:     clientID,
		Version:  "test",
		Revision: revision,
		Mappings: mappings,
	}))

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := control.ReadMessage(conn)
	require.NoError(t, err)
	_ = conn.SetReadDeadline(time.Time{})
	ack, ok := msg.(*control.HelloAck)
	require.True(t, ok, "expected hello_ack, got %T", msg)
	require.True(t, ack.OK, "hello rejected: %s", ack.Error)
	return ack
}

// rawReadPush reads one push message.
func rawReadPush(t *testing.T, conn net.Conn) *control.Push {
	t.Helper()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := control.ReadMessage(conn)
	require.NoError(t, err)
	_ = conn.SetReadDeadline(time.Time{})
	push, ok := msg.(*control.Push)
	require.True(t, ok, "expected push, got %T", msg)
	return push
}

// TestPushTimeoutClosesControlStreamAndResyncs drops a push ack and verifies
// that the server closes the control stream instead of leaving a revision
// split, then accepts a retry after the client re-registers. The retry also
// proves a stale ack with the same revision but a different push ID is ignored.
func TestPushTimeoutClosesControlStreamAndResyncs(t *testing.T) {
	oldTimeout := pushAckTimeout
	pushAckTimeout = time.Second
	defer func() { pushAckTimeout = oldTimeout }()

	srv, stop := startTestServer(t)
	defer stop()

	m1Port := freeTCPPort(t)
	m1 := config.Mapping{ID: "m1", Type: "tcp", RemotePort: m1Port, LocalAddr: "127.0.0.1:9001", Enabled: true}
	hy := newHY2Client(t, srv, "pw", nil)

	conn := openRawControl(t, hy)
	defer conn.Close()
	rawHello(t, conn, "raw-push", 1, []config.Mapping{m1})
	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		views := srv.Clients()
		return len(views) == 1 && len(views[0].Mappings) == 1 && views[0].Mappings[0].Status == "running"
	}))

	m2 := config.Mapping{ID: "m2", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true}
	pushDone := make(chan error, 1)
	go func() {
		_, err := srv.PushMappings("raw-push", 1, []config.Mapping{m1, m2})
		pushDone <- err
	}()

	push := rawReadPush(t, conn)
	require.NotEmpty(t, push.ID)
	// Deliberately do not acknowledge the push.
	require.ErrorIs(t, <-pushDone, control.ErrPushTimeout)

	// The server must close the stream so the client reconnects and re-sends
	// its authoritative configuration in hello.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := control.ReadMessage(conn)
	require.Error(t, err, "push timeout must close the control stream")
	require.NoError(t, waitFor(t, 2*time.Second, func() bool {
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", m1Port))
		if err != nil {
			return false
		}
		_ = listener.Close()
		return true
	}), "the timed-out push must release the old listener after the stream closes")

	// Reconnect and retry. A stale ack for the same revision but another push
	// ID must not satisfy the retry; the matching ack must.
	conn2 := openRawControl(t, hy)
	defer conn2.Close()
	rawHello(t, conn2, "raw-push", 1, []config.Mapping{m1})

	pushDone2 := make(chan error, 1)
	go func() {
		_, err := srv.PushMappings("raw-push", 1, []config.Mapping{m1, m2})
		pushDone2 <- err
	}()
	push2 := rawReadPush(t, conn2)

	require.NoError(t, control.WriteMessage(conn2, &control.PushAck{
		Type:     control.TypePushAck,
		ID:       "stale-push-id",
		OK:       true,
		Revision: push2.Revision,
	}))
	require.NoError(t, control.WriteMessage(conn2, &control.PushAck{
		Type:     control.TypePushAck,
		ID:       push2.ID,
		OK:       true,
		Revision: push2.Revision,
	}))
	require.NoError(t, <-pushDone2)

	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		views := srv.Clients()
		return len(views) == 1 && len(views[0].Mappings) == 2 && views[0].Revision == 2
	}))
}

// TestRejectedUpdateClosesStreamAndResyncs deletes a mapping and adds one that
// cannot bind in a single client update. The rejected transaction must not
// leave the deleted mapping's public listener running: the server closes the
// stream, the client's hello re-registers its authoritative config, and the
// removed listener is released.
func TestRejectedUpdateClosesStreamAndResyncs(t *testing.T) {
	srv, stop := startTestServer(t)
	defer stop()

	m1Port := freeTCPPort(t)
	m1 := config.Mapping{ID: "m1", Type: "tcp", RemotePort: m1Port, LocalAddr: "127.0.0.1:9001", Enabled: true}
	hy := newHY2Client(t, srv, "pw", nil)

	conn := openRawControl(t, hy)
	defer conn.Close()
	rawHello(t, conn, "raw-update", 1, []config.Mapping{m1})
	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		views := srv.Clients()
		return len(views) == 1 && len(views[0].Mappings) == 1 && views[0].Mappings[0].Status == "running"
	}))

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	occupiedPort := occupied.Addr().(*net.TCPAddr).Port

	// Revision 2 removes m1 and adds m2 on a port that cannot be bound.
	failing := config.Mapping{ID: "m2", Type: "tcp", RemotePort: occupiedPort, LocalAddr: "127.0.0.1:9002", Enabled: true}
	require.NoError(t, control.WriteMessage(conn, &control.Update{
		Type:     control.TypeUpdate,
		Revision: 2,
		Mappings: []config.Mapping{failing},
	}))

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := control.ReadMessage(conn)
	require.NoError(t, err)
	ack, ok := msg.(*control.UpdateAck)
	require.True(t, ok, "expected update_ack, got %T", msg)
	require.Equal(t, int64(1), ack.Revision)
	require.Len(t, ack.Results, 1)
	require.Equal(t, "port_in_use", ack.Results[0].Error)

	// The rejected update forces a resync.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = control.ReadMessage(conn)
	require.Error(t, err, "a rejected update must close the control stream")
	require.NoError(t, waitFor(t, 2*time.Second, func() bool {
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", m1Port))
		if err != nil {
			return false
		}
		_ = listener.Close()
		return true
	}), "the deleted mapping's listener must be closed after the resync")

	// The client reconnects with its authoritative configuration: m1 is gone
	// and the failing m2 is still present.
	conn2 := openRawControl(t, hy)
	defer conn2.Close()
	rawHello(t, conn2, "raw-update", 2, []config.Mapping{failing})
	require.NoError(t, waitFor(t, 5*time.Second, func() bool {
		views := srv.Clients()
		if len(views) != 1 || views[0].Revision != 2 || len(views[0].Mappings) != 1 {
			return false
		}
		mapping := views[0].Mappings[0]
		return mapping.ID == "m2" && mapping.Status == "failed" && mapping.Error == "port_in_use"
	}))

	// The old listener stays released.
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", m1Port))
	require.NoError(t, err)
	require.NoError(t, listener.Close())
}
