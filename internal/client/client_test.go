package client

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
)

func validTestClientConfig() *config.ClientConfig {
	return &config.ClientConfig{
		Server: "127.0.0.1:443",
		Auth:   "pw",
		TLS:    config.ClientTLSConfig{Insecure: true},
		Control: config.ControlConfig{
			Token: "0123456789abcdef0123456789abcdef",
		},
		Client: config.ClientIdentity{Name: "test"},
		Web:    config.WebConfig{Listen: ""},
		Log:    config.LogConfig{Level: "info"},
	}
}

func TestMappingViewsMergesServerResults(t *testing.T) {
	mappings := []config.Mapping{
		{ID: "tcp1", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9001", Enabled: true},
		{ID: "udp1", Type: "udp", RemotePort: 0, LocalAddr: "127.0.0.1:9002", Enabled: true},
		{ID: "off", Type: "tcp", RemotePort: 0, LocalAddr: "127.0.0.1:9003", Enabled: false},
	}
	results := map[string]control.Result{
		"tcp1": {ID: "tcp1", OK: true, AssignedPort: 23001},
		"udp1": {ID: "udp1", Error: "port_in_use"},
	}

	views := mappingViews(mappings, results)
	require.Len(t, views, 3)
	require.Equal(t, "running", views[0].Status)
	require.Equal(t, 23001, views[0].AssignedPort)
	require.Equal(t, "failed", views[1].Status)
	require.Equal(t, "port_in_use", views[1].Error)
	require.Equal(t, "disabled", views[2].Status)
}

func TestMappingViewsBeforeHandshakeIsPending(t *testing.T) {
	mappings := []config.Mapping{
		{ID: "m1", Type: "tcp", LocalAddr: "127.0.0.1:9001", Enabled: true},
	}
	views := mappingViews(mappings, nil)
	require.Len(t, views, 1)
	require.Equal(t, "pending", views[0].Status)
}

func TestNewGeneratesClientID(t *testing.T) {
	cfg := validTestClientConfig()
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	require.NotEmpty(t, cfg.Client.ID)
	require.Regexp(t, `^c-[0-9a-f]{16}$`, cfg.Client.ID)
	require.NoError(t, cl.Close())
}

func TestNewRejectsInvalidObfs(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Obfs.Type = "bogus"
	_, err := New(cfg, nil)
	require.ErrorContains(t, err, "unsupported obfs.type")
}

func TestNewRejectsShortSalamanderPassword(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Obfs.Type = "salamander"
	cfg.Obfs.Salamander.Password = "abc"
	_, err := New(cfg, nil)
	require.Error(t, err)
}

func TestWaitReadyAfterClose(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	require.NoError(t, cl.Close())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.Error(t, cl.WaitReady(ctx))
}

func TestParsePin(t *testing.T) {
	valid := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	pin, err := parsePin(valid)
	require.NoError(t, err)
	require.Len(t, pin, 32)

	withPrefix, err := parsePin("sha256:" + valid)
	require.NoError(t, err)
	require.Equal(t, pin, withPrefix)

	withColons, err := parsePin("01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef:01:23:45:67:89:ab:cd:ef")
	require.NoError(t, err)
	require.Equal(t, pin, withColons)

	_, err = parsePin("not-a-pin")
	require.Error(t, err)
}

// stalledHY2Client simulates a HY2 client whose TCP stream request never
// completes until Close is called.
type stalledHY2Client struct {
	tcpStarted chan struct{}
	closed     chan struct{}
	closeOnce  sync.Once
	tcpOnce    sync.Once
}

func newStalledHY2Client() *stalledHY2Client {
	return &stalledHY2Client{
		tcpStarted: make(chan struct{}),
		closed:     make(chan struct{}),
	}
}

func (f *stalledHY2Client) TCP(string) (net.Conn, error) {
	f.tcpOnce.Do(func() { close(f.tcpStarted) })
	<-f.closed
	return nil, fmt.Errorf("control stream closed")
}

func (f *stalledHY2Client) UDP() (hyclient.HyUDPConn, error) {
	return nil, fmt.Errorf("UDP is not used by this test")
}

func (f *stalledHY2Client) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *stalledHY2Client) isClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}

func clientWithStalledHY2(t *testing.T) (*Client, *stalledHY2Client) {
	t.Helper()

	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	fake := newStalledHY2Client()
	cl.newHY2 = func(*hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error) {
		return fake, nil, nil
	}
	return cl, fake
}

func TestCloseStopsStalledControlStream(t *testing.T) {
	cl, fake := clientWithStalledHY2(t)
	done := make(chan error, 1)
	go func() { done <- cl.Start(context.Background()) }()

	select {
	case <-fake.tcpStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("control stream request did not start")
	}

	require.NoError(t, cl.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Close")
	}
	require.True(t, fake.isClosed())
}

func TestShutdownStopsStalledControlStream(t *testing.T) {
	cl, fake := clientWithStalledHY2(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cl.Start(ctx) }()

	select {
	case <-fake.tcpStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("control stream request did not start")
	}

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
	require.True(t, fake.isClosed())
	require.NoError(t, cl.Close())
}

func TestShutdownClosesAbandonedHY2Client(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)

	fake := newStalledHY2Client()
	release := make(chan struct{})
	factoryStarted := make(chan struct{})
	cl.newHY2 = func(*hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error) {
		close(factoryStarted)
		<-release
		return fake, nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cl.Start(ctx) }()

	select {
	case <-factoryStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("HY2 client factory did not start")
	}

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}

	close(release)
	require.Eventually(t, fake.isClosed, 2*time.Second, 10*time.Millisecond)
	require.NoError(t, cl.Close())
}

// stalledHandshakeHY2Client returns a control stream that never receives a
// HelloAck, so the client blocks in the handshake read.
type stalledHandshakeHY2Client struct {
	tcpStarted chan struct{}
	closed     chan struct{}
	closeOnce  sync.Once
	tcpOnce    sync.Once
}

func newStalledHandshakeHY2Client() *stalledHandshakeHY2Client {
	return &stalledHandshakeHY2Client{
		tcpStarted: make(chan struct{}),
		closed:     make(chan struct{}),
	}
}

func (f *stalledHandshakeHY2Client) TCP(string) (net.Conn, error) {
	f.tcpOnce.Do(func() { close(f.tcpStarted) })
	local, remote := net.Pipe()
	go func() {
		<-f.closed
		_ = remote.Close()
	}()
	return local, nil
}

func (f *stalledHandshakeHY2Client) UDP() (hyclient.HyUDPConn, error) {
	return nil, fmt.Errorf("UDP is not used by this test")
}

func (f *stalledHandshakeHY2Client) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *stalledHandshakeHY2Client) isClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}

func TestCloseAndShutdownStopStalledHandshake(t *testing.T) {
	run := func(t *testing.T, cancelContext bool) {
		t.Helper()

		cl, err := New(validTestClientConfig(), nil)
		require.NoError(t, err)
		fake := newStalledHandshakeHY2Client()
		cl.newHY2 = func(*hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error) {
			return fake, nil, nil
		}

		ctx := context.Background()
		var cancel context.CancelFunc
		if cancelContext {
			ctx, cancel = context.WithCancel(ctx)
			defer cancel()
		}

		done := make(chan error, 1)
		go func() { done <- cl.Start(ctx) }()

		select {
		case <-fake.tcpStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("control stream request did not start")
		}

		if cancelContext {
			cancel()
		} else {
			require.NoError(t, cl.Close())
		}
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("Start did not return after shutdown")
		}
		require.True(t, fake.isClosed())
		require.NoError(t, cl.Close())
	}

	t.Run("close", func(t *testing.T) { run(t, false) })
	t.Run("shutdown", func(t *testing.T) { run(t, true) })
}
