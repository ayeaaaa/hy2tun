package client

import (
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"
	xproxy "golang.org/x/net/proxy"

	"hy2tun/internal/testutil"
)

// directHY2Client is a HY2 client test double whose TCP streams connect
// directly to the requested address.
type directHY2Client struct {
	mu      sync.Mutex
	targets []string
}

func (f *directHY2Client) TCP(addr string) (net.Conn, error) {
	f.mu.Lock()
	f.targets = append(f.targets, addr)
	f.mu.Unlock()
	return net.Dial("tcp", addr)
}

func (f *directHY2Client) UDP() (hyclient.HyUDPConn, error) {
	return nil, fmt.Errorf("UDP is not used by this test")
}

func (f *directHY2Client) Close() error {
	return nil
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())
	return port
}

func TestNewBindsConfiguredProxyListeners(t *testing.T) {
	socksPort := freeTCPPort(t)
	httpPort := freeTCPPort(t)
	cfg := validTestClientConfig()
	cfg.Proxy.SOCKS5 = fmt.Sprintf("127.0.0.1:%d", socksPort)
	cfg.Proxy.SOCKS5Auth = "alice:s3cret"
	cfg.Proxy.HTTP = fmt.Sprintf("127.0.0.1:%d", httpPort)
	cfg.Proxy.HTTPAuth = "bob:hunter2"

	cl, err := New(cfg, nil)
	require.NoError(t, err)
	require.Len(t, cl.proxies, 2)
	require.Equal(t, cfg.Proxy.SOCKS5, cl.proxies[0].Addr())
	require.Equal(t, cfg.Proxy.HTTP, cl.proxies[1].Addr())

	require.NoError(t, cl.Close())
	conn, err := net.DialTimeout("tcp", cl.proxies[0].Addr(), 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("SOCKS5 listener still accepts after Close")
	}
}

func TestClientProxyDialsThroughHY2TCP(t *testing.T) {
	echoAddr, stop := testutil.StartTCPEcho(t)
	defer stop()

	cfg := validTestClientConfig()
	cfg.Proxy.SOCKS5 = fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer cl.Close()

	fake := &directHY2Client{}
	cl.mu.Lock()
	cl.hy = fake
	cl.mu.Unlock()
	cl.startProxies()

	p, err := xproxy.SOCKS5("tcp", cl.proxies[0].Addr(), nil, xproxy.Direct)
	require.NoError(t, err)
	conn, err := p.Dial("tcp", echoAddr)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf))

	fake.mu.Lock()
	targets := append([]string(nil), fake.targets...)
	fake.mu.Unlock()
	require.Contains(t, targets, echoAddr)
}
