package client

import (
	"context"
	"net"
	"sync"
	"testing"
	"testing/synctest"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/stretchr/testify/require"

	"hy2tun/internal/transport"
)

type blockedLateUDP struct {
	*fakeUDPConn
	entered, release chan struct{}
	once             sync.Once
}

func (d *blockedLateUDP) Close() error {
	d.once.Do(func() { close(d.entered); <-d.release; _ = d.fakeUDPConn.Close() })
	return nil
}

func TestHY2ReplacementRetainsNativeAndCleanupBudget(t *testing.T) {
	for _, kind := range []string{"proxy", "reverse", "udp"} {
		for _, cancelRequest := range []bool{false, true} {
			name := kind + "/owner_cancel"
			if cancelRequest {
				name = kind + "/request_then_owner_cancel"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					started, nativeRelease := make(chan struct{}), make(chan struct{})
					cleanupEntered, cleanupRelease := make(chan struct{}), make(chan struct{})
					var nativeOnce, cleanupOnce sync.Once
					defer nativeOnce.Do(func() { close(nativeRelease) })
					defer cleanupOnce.Do(func() { close(cleanupRelease) })
					late, latePeer := net.Pipe()
					defer late.Close()
					defer latePeer.Close()
					child := &blockedChildClose{Conn: late, entered: cleanupEntered, release: cleanupRelease}
					udp := &blockedLateUDP{fakeUDPConn: newFakeUDPConn(), entered: cleanupEntered, release: cleanupRelease}
					oldRoot := &gatedDataHY2{closed: make(chan struct{})}
					oldRoot.tcp = func(string) (net.Conn, error) { close(started); <-nativeRelease; return child, nil }
					oldRoot.udp = func() (hyclient.HyUDPConn, error) { close(started); <-nativeRelease; return udp, nil }
					cl, _ := installGatedHY2(t, oldRoot)
					cl.hy2DataOpenSlots = make(chan struct{}, 1)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					session, _, ownerCtx := cl.sessionFor(nil)
					done := make(chan error, 1)
					go func() {
						var err error
						switch kind {
						case "proxy":
							_, err = cl.TCPContext(ctx, "old.example:443")
						case "reverse":
							_, err = cl.openTCPData(ctx, ownerCtx, session, transport.TCPRequest{ReverseToken: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
						case "udp":
							_, err = cl.openUDPData(ctx, ownerCtx, session, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
						}
						done <- err
					}()
					<-started
					if cancelRequest {
						cancel()
						require.ErrorIs(t, <-done, context.Canceled)
					}
					// Replacement must remain prompt, but cannot reclaim the old
					// reservation while native work/cleanup remains outstanding.
					for i := 0; i < 3; i++ {
						live, peer := net.Pipe()
						defer live.Close()
						defer peer.Close()
						root := &gatedDataHY2{closed: make(chan struct{})}
						root.tcp = func(string) (net.Conn, error) { return live, nil }
						owner, controlPeer := net.Pipe()
						defer controlPeer.Close()
						require.True(t, cl.setCurrent(transport.WrapHY2(root, cl.cfg.Control.Token), owner))
						cl.markConnected()
						if i == 0 && !cancelRequest {
							require.ErrorIs(t, <-done, context.Canceled)
						}
						synctest.Wait()
						conn, err := cl.TCP("new.example:443")
						if conn != nil {
							_ = conn.Close()
						}
						require.ErrorIs(t, err, transport.ErrQueueFull, "replacement admitted work before old native operation returned")
					}
					nativeOnce.Do(func() { close(nativeRelease) })
					<-cleanupEntered
					synctest.Wait()
					conn, err := cl.TCP("new.example:443")
					if conn != nil {
						_ = conn.Close()
					}
					require.ErrorIs(t, err, transport.ErrQueueFull, "native return alone must not reclaim blocked late-child cleanup")
					cleanupOnce.Do(func() { close(cleanupRelease) })
					synctest.Wait()
					conn, err = cl.TCP("new.example:443")
					require.NoError(t, err, "actual native and cleanup completion must restore capacity")
					require.NoError(t, conn.Close())
				})
			})
		}
	}
}
