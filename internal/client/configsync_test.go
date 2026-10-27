package client

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/view"
)

// writeTempClientConfig writes a valid client configuration with one TCP
// mapping and returns its path.
func writeTempClientConfig(t *testing.T, revision int64) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "client.yaml")
	cfg := &config.ClientConfig{
		Server:  "127.0.0.1:443",
		Auth:    "pw",
		TLS:     config.ClientTLSConfig{Insecure: true},
		Control: config.ControlConfig{Token: "0123456789abcdef0123456789abcdef"},
		Client:  config.ClientIdentity{ID: "c-test", Name: "test", Revision: revision},
		Mappings: []config.Mapping{
			{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
		},
		Log: config.LogConfig{Level: "info"},
	}
	require.NoError(t, config.SaveClient(path, cfg))
	return path
}

// loadClientConfig loads a client configuration from path.
func loadClientConfig(t *testing.T, path string) *config.ClientConfig {
	t.Helper()

	cfg, err := config.LoadClient(path)
	require.NoError(t, err)
	return cfg
}

func TestMappingEditErrorsUseSentinels(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 1)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	err = cl.UpdateMapping(config.Mapping{
		ID: "missing", Type: "tcp", LocalAddr: "127.0.0.1:9002", Enabled: true,
	})
	require.ErrorIs(t, err, config.ErrMappingNotFound)

	err = cl.DeleteMapping("missing")
	require.ErrorIs(t, err, config.ErrMappingNotFound)

	err = cl.AddMapping(config.Mapping{
		ID: "m1", Type: "tcp", LocalAddr: "127.0.0.1:9002", Enabled: true,
	})
	require.ErrorIs(t, err, config.ErrMappingExists)
}

func TestAddMappingWritesConfigAndBumpsRevision(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 3)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()
	require.Equal(t, cfgPath, cl.ConfigPath())
	require.Equal(t, int64(3), cl.Revision())

	require.NoError(t, cl.AddMapping(config.Mapping{
		ID: "m2", Type: "tcp", RemotePort: 0,
		LocalAddr: "127.0.0.1:9002", Note: "new", Enabled: true,
	}))

	require.Equal(t, int64(4), cl.Revision())
	saved, err := config.LoadClient(cfgPath)
	require.NoError(t, err)
	require.Len(t, saved.Mappings, 2)
	require.Equal(t, int64(4), saved.Client.Revision)
	info, err := os.Stat(cfgPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestUpdateMappingWritesConfigAndBumpsRevision(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 3)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	require.NoError(t, cl.UpdateMapping(config.Mapping{
		ID: "m1", Type: "tcp", RemotePort: 0,
		LocalAddr: "127.0.0.1:9999", Note: "changed", Enabled: true,
	}))

	require.Equal(t, int64(4), cl.Revision())
	saved, err := config.LoadClient(cfgPath)
	require.NoError(t, err)
	require.Len(t, saved.Mappings, 1)
	require.Equal(t, "127.0.0.1:9999", saved.Mappings[0].LocalAddr)
	require.Equal(t, "changed", saved.Mappings[0].Note)
	require.Equal(t, int64(4), saved.Client.Revision)
}

func TestDeleteMappingWritesConfigAndBumpsRevision(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 3)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	require.NoError(t, cl.DeleteMapping("m1"))

	require.Equal(t, int64(4), cl.Revision())
	require.Empty(t, cl.Mappings())
	saved, err := config.LoadClient(cfgPath)
	require.NoError(t, err)
	require.Empty(t, saved.Mappings)
	require.Equal(t, int64(4), saved.Client.Revision)
}

func TestLocalEditWriteFailureLeavesMemoryUnchanged(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 3)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("not a directory"), 0600))
	cl.configPath = filepath.Join(blocker, "client.yaml")

	err = cl.AddMapping(config.Mapping{
		ID: "m2", Type: "tcp", RemotePort: 0,
		LocalAddr: "127.0.0.1:9002", Enabled: true,
	})
	require.Error(t, err)
	require.Equal(t, int64(3), cl.Revision())
	require.Len(t, cl.Mappings(), 1)

	saved, err := config.LoadClient(cfgPath)
	require.NoError(t, err)
	require.Len(t, saved.Mappings, 1)
	require.Equal(t, int64(3), saved.Client.Revision)
}

// pushThroughPipe runs handlePush against one end of a net.Pipe and returns
// the PushAck written to the other end.
func pushThroughPipe(t *testing.T, cl *Client, push *control.Push) *control.PushAck {
	t.Helper()

	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()

	ackCh := make(chan *control.PushAck, 1)
	go func() {
		msg, err := control.ReadMessage(remote)
		if err != nil {
			ackCh <- nil
			return
		}
		ack, _ := msg.(*control.PushAck)
		ackCh <- ack
	}()

	cl.handlePush(local, push)

	select {
	case ack := <-ackCh:
		require.NotNil(t, ack)
		return ack
	case <-time.After(2 * time.Second):
		t.Fatal("push ack was not written")
		return nil
	}
}

func TestSendUpdateWaitsForHandshake(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 3)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	cl.mu.Lock()
	cl.controlConn = local
	cl.mu.Unlock()

	// A local edit while hello is in flight must not send an Update before the
	// handshake completes.
	require.NoError(t, cl.AddMapping(config.Mapping{
		ID: "m2", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true,
	}))
	require.Equal(t, int64(4), cl.Revision())

	require.NoError(t, remote.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	_, err = control.ReadMessage(remote)
	require.Error(t, err, "update must not be sent before the handshake completes")
	require.NoError(t, remote.SetReadDeadline(time.Time{}))

	cl.markConnected()
	go cl.sendUpdate()
	msg, err := control.ReadMessage(remote)
	require.NoError(t, err)
	update, ok := msg.(*control.Update)
	require.True(t, ok)
	require.Equal(t, int64(4), update.Revision)
	require.Len(t, update.Mappings, 2)
	require.Equal(t, "m2", update.Mappings[1].ID)
}

func TestHelloAckPreservesConcurrentLocalEdit(t *testing.T) {
	tests := []struct {
		name             string
		results          []control.Result
		wantAssigned     int
		wantPortAfterAck int
	}{
		{
			name:             "assigned port",
			results:          []control.Result{{ID: "m1", OK: true, AssignedPort: 21001}},
			wantAssigned:     21001,
			wantPortAfterAck: 21001,
		},
		{
			name:             "no port change",
			results:          []control.Result{{ID: "m1", OK: true}},
			wantAssigned:     0,
			wantPortAfterAck: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := writeTempClientConfig(t, 3)
			cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
			require.NoError(t, err)
			defer cl.Close()

			local, remote := net.Pipe()
			defer local.Close()
			defer remote.Close()
			cl.mu.Lock()
			cl.controlConn = local
			cl.mu.Unlock()

			snapshot := cl.snapshotConfig()
			require.NoError(t, cl.AddMapping(config.Mapping{
				ID: "m2", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true,
			}))
			require.Equal(t, int64(4), cl.Revision())

			updateCh := make(chan *control.Update, 1)
			go func() {
				msg, err := control.ReadMessage(remote)
				if err != nil {
					updateCh <- nil
					return
				}
				update, _ := msg.(*control.Update)
				updateCh <- update
			}()

			cl.markConnected()
			require.NoError(t, cl.applyHelloAck(snapshot, &control.HelloAck{
				OK:      true,
				Results: tc.results,
			}))

			var update *control.Update
			select {
			case update = <-updateCh:
			case <-time.After(2 * time.Second):
				t.Fatal("hello ack did not announce the current config")
			}
			require.NotNil(t, update)
			require.Equal(t, int64(4), update.Revision)
			require.Len(t, update.Mappings, 2)
			require.Equal(t, "m2", update.Mappings[1].ID)
			require.Equal(t, 0, update.Mappings[0].RemotePort)

			require.Equal(t, int64(4), cl.Revision())
			views := cl.Mappings()
			require.Len(t, views, 2)
			require.Equal(t, tc.wantAssigned, views[0].AssignedPort)
			require.Equal(t, "m2", views[1].ID)

			// The hello write-back must not overwrite the local edit.
			saved, err := config.LoadClient(cfgPath)
			require.NoError(t, err)
			require.Equal(t, int64(4), saved.Client.Revision)
			require.Len(t, saved.Mappings, 2)
			require.Equal(t, "m2", saved.Mappings[1].ID)
			require.Equal(t, 0, saved.Mappings[0].RemotePort)

			// The server's update ack carries the assigned port; it is written
			// without a second revision bump.
			cl.handleUpdateAck(&control.UpdateAck{
				Revision: 4,
				Results:  tc.results,
			})
			require.Equal(t, int64(4), cl.Revision())
			savedAfter, err := config.LoadClient(cfgPath)
			require.NoError(t, err)
			require.Equal(t, int64(4), savedAfter.Client.Revision)
			require.Len(t, savedAfter.Mappings, 2)
			require.Equal(t, "m2", savedAfter.Mappings[1].ID)
			require.Equal(t, tc.wantPortAfterAck, savedAfter.Mappings[0].RemotePort)
		})
	}
}

func TestRejectedUpdateAckPreservesRunningMapping(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 3)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	// Simulate a local edit that added a UDP mapping and a second mapping
	// whose bind failed on the server.
	cl.configMu.Lock()
	next := cloneClientConfig(cl.cfg)
	next.Client.Revision = 4
	next.Mappings = []config.Mapping{
		{ID: "m1", Type: "udp", RemotePort: 21001, LocalAddr: "127.0.0.1:9001", Enabled: true},
		{ID: "m2", Type: "tcp", RemotePort: 21002, LocalAddr: "127.0.0.1:9002", Enabled: true},
	}
	cl.cfg = next
	cl.configMu.Unlock()

	cl.mu.Lock()
	cl.mappings = []view.MappingView{
		{Mapping: next.Mappings[0], Status: "running", AssignedPort: 21001},
		{Mapping: next.Mappings[1], Status: "pending"},
	}
	cl.udpTokens = map[string]string{"m1": "0123456789abcdef0123456789abcdef"}
	cl.mu.Unlock()

	cl.handleUpdateAck(&control.UpdateAck{
		Revision: 3,
		Results: []control.Result{
			{ID: "m1", Error: string(control.CodeInternal)},
			{ID: "m2", Error: string(control.CodePortInUse)},
		},
	})

	require.Equal(t, int64(4), cl.Revision())
	views := cl.Mappings()
	require.Len(t, views, 2)
	require.Equal(t, "running", views[0].Status)
	require.Equal(t, "failed", views[1].Status)
	require.Equal(t, string(control.CodePortInUse), views[1].Error)

	cl.mu.Lock()
	token := cl.udpTokens["m1"]
	cl.mu.Unlock()
	require.Equal(t, "0123456789abcdef0123456789abcdef", token)
}

func TestHandlePushConflictKeepsRevision(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 3)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	ack := pushThroughPipe(t, cl, &control.Push{
		Type:         control.TypePush,
		BaseRevision: 2,
		Revision:     4,
		Mappings: []config.Mapping{
			{ID: "m2", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true},
		},
	})
	require.False(t, ack.OK)
	require.Equal(t, "conflict", ack.Error)
	require.Equal(t, int64(3), ack.Revision)
	require.Equal(t, int64(3), cl.Revision())

	saved, err := config.LoadClient(cfgPath)
	require.NoError(t, err)
	require.Len(t, saved.Mappings, 1)
	require.Equal(t, "m1", saved.Mappings[0].ID)
	require.Equal(t, int64(3), saved.Client.Revision)
}

func TestHandlePushWriteFailureRollsBack(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 3)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	blocker := filepath.Join(t.TempDir(), "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("not a directory"), 0600))
	cl.configPath = filepath.Join(blocker, "client.yaml")

	ack := pushThroughPipe(t, cl, &control.Push{
		Type:         control.TypePush,
		BaseRevision: 3,
		Revision:     4,
		Mappings: []config.Mapping{
			{ID: "m1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Note: "pushed", Enabled: true},
			{ID: "m2", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true},
		},
	})
	require.False(t, ack.OK)
	require.Equal(t, "write_failed", ack.Error)
	require.Equal(t, int64(3), ack.Revision)
	require.Equal(t, int64(3), cl.Revision())
	require.Len(t, cl.Mappings(), 1)

	saved, err := config.LoadClient(cfgPath)
	require.NoError(t, err)
	require.Len(t, saved.Mappings, 1)
	require.Equal(t, int64(3), saved.Client.Revision)
}

func TestHandlePushAppliesRevisionExactly(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 3)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	ack := pushThroughPipe(t, cl, &control.Push{
		Type:         control.TypePush,
		BaseRevision: 3,
		Revision:     7,
		Mappings: []config.Mapping{
			{ID: "m1", Type: "tcp", RemotePort: 21001, LocalAddr: "127.0.0.1:9001", Note: "pushed", Enabled: true},
			{ID: "m2", Type: "tcp", RemotePort: 21002, LocalAddr: "127.0.0.1:9002", Enabled: true},
		},
	})
	require.True(t, ack.OK)
	require.Equal(t, int64(7), ack.Revision)
	require.Equal(t, int64(7), cl.Revision())

	saved, err := config.LoadClient(cfgPath)
	require.NoError(t, err)
	require.Equal(t, int64(7), saved.Client.Revision)
	require.Len(t, saved.Mappings, 2)
	require.Equal(t, "pushed", saved.Mappings[0].Note)
	require.Equal(t, 21002, saved.Mappings[1].RemotePort)
}

// fakePushHY2 is a minimal HY2 client for push UDP tests.
type fakePushHY2 struct {
	udp       *fakePushUDPConn
	closeOnce sync.Once
	closed    chan struct{}
}

func newFakePushHY2() *fakePushHY2 {
	return &fakePushHY2{
		udp:    newFakePushUDPConn(),
		closed: make(chan struct{}),
	}
}

func (f *fakePushHY2) TCP(string) (net.Conn, error) {
	return nil, fmt.Errorf("TCP is not used by this test")
}

func (f *fakePushHY2) UDP() (hyclient.HyUDPConn, error) {
	return f.udp, nil
}

func (f *fakePushHY2) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

type fakePushUDPConn struct {
	closeOnce sync.Once
	closed    chan struct{}
	mu        sync.Mutex
	sends     []string
}

func newFakePushUDPConn() *fakePushUDPConn {
	return &fakePushUDPConn{closed: make(chan struct{})}
}

func (c *fakePushUDPConn) Receive() ([]byte, string, error) {
	<-c.closed
	return nil, "", net.ErrClosed
}

func (c *fakePushUDPConn) Send(_ []byte, addr string) error {
	c.mu.Lock()
	c.sends = append(c.sends, addr)
	c.mu.Unlock()
	return nil
}

func (c *fakePushUDPConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func TestHandlePushStartsUDPWithServerToken(t *testing.T) {
	cfgPath := writeTempClientConfig(t, 3)
	cl, err := New(loadClientConfig(t, cfgPath), slog.Default())
	require.NoError(t, err)
	defer cl.Close()

	fake := newFakePushHY2()
	cl.mu.Lock()
	cl.hy = fake
	cl.mu.Unlock()

	token := "0123456789abcdef0123456789abcdef"
	ack := pushThroughPipe(t, cl, &control.Push{
		Type:         control.TypePush,
		BaseRevision: 3,
		Revision:     4,
		Mappings: []config.Mapping{
			{ID: "u1", Type: "udp", RemotePort: 21001, LocalAddr: "127.0.0.1:9002", Enabled: true},
		},
		Results: []control.Result{
			{ID: "u1", OK: true, AssignedPort: 21001, UDPToken: token},
		},
	})
	require.True(t, ack.OK)
	require.Equal(t, int64(4), ack.Revision)

	views := cl.Mappings()
	require.Len(t, views, 1)
	require.Equal(t, "running", views[0].Status)
	require.Equal(t, 21001, views[0].AssignedPort)

	cl.mu.Lock()
	udp := cl.udpMappings["u1"]
	cl.mu.Unlock()
	require.NotNil(t, udp, "push must start a reverse UDP session")
	require.Equal(t, udpVirtualAddress(token), udp.virtualAddr)

	fake.udp.mu.Lock()
	sends := append([]string(nil), fake.udp.sends...)
	fake.udp.mu.Unlock()
	require.Contains(t, sends, udpVirtualAddress(token))
}
