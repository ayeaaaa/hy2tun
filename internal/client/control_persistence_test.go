package client

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/transport"
	"hy2tun/internal/view"
)

func TestHeartbeatPongsAndQueuedControlDuringHelloPersistence(t *testing.T) {
	for _, invalidate := range []string{"none", "close", "epoch"} {
		t.Run(invalidate, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				clock := newPolicyClock()
				cfg := validTestClientConfig()
				cfg.Mappings = []config.Mapping{{ID: "a", Type: "tcp", LocalAddr: "127.0.0.1:9999", Enabled: true}}
				cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Clock: clock})
				require.NoError(t, err)
				entered, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				var writes, pongs atomic.Int32
				cl.SetPersist(func(*config.ClientConfig) error {
					if writes.Add(1) == 1 {
						close(entered)
						<-release
					}
					return nil
				})
				s := newSessionStub(transport.HY2)
				conn, peer := net.Pipe()
				s.control = conn
				done := make(chan error, 1)
				peerDone := make(chan struct{})
				queued := make(chan struct{})
				replies := make(chan any, 20)
				go func() {
					defer close(peerDone)
					defer peer.Close()
					if _, err := control.ReadMessage(peer); err != nil {
						return
					}
					if control.WriteMessage(peer, &control.HelloAck{Type: control.TypeHelloAck, OK: true, Results: []control.Result{{ID: "a", OK: true, AssignedPort: 23456}}}) != nil {
						return
					}
					for {
						msg, err := control.ReadMessage(peer)
						if err != nil {
							return
						}
						if ping, ok := msg.(*control.Ping); ok {
							if control.WriteMessage(peer, &control.Pong{Type: control.TypePong, TS: ping.TS}) != nil {
								return
							}
							n := pongs.Add(1)
							if n == 1 {
								if control.WriteMessage(peer, &control.Push{Type: control.TypePush, ID: "queued", BaseRevision: 1, Revision: 2, Mappings: []config.Mapping{{ID: "a", Type: "tcp", LocalAddr: "127.0.0.1:9998", RemotePort: 23456, Enabled: true}}}) != nil {
									return
								}
								if control.WriteMessage(peer, &control.UpdateAck{Type: control.TypeUpdateAck, Revision: 2, Results: []control.Result{{ID: "a", OK: true, AssignedPort: 23456}}}) != nil {
									return
								}
								close(queued)
							}
						} else {
							replies <- msg
						}
					}
				}()
				go func() { _, err := cl.runSession(context.Background(), s); done <- err }()
				<-entered
				synctest.Wait()
				defer func() {
					releaseOnce.Do(func() { close(release) })
					cl.Close()
					peer.Close()
					<-done
					<-peerDone
					synctest.Wait()
				}()
				for range 5 {
					clock.advance(10 * time.Second)
				}
				select {
				case <-s.closed:
					t.Fatal("local persistence falsely expired a peer sending matching Pongs")
				default:
				}
				require.Equal(t, int32(5), pongs.Load())
				require.Equal(t, int32(1), writes.Load())
				<-queued
				if invalidate == "close" {
					cl.Close()
				} else if invalidate == "epoch" {
					cl.forceReconnect()
				}
				releaseOnce.Do(func() { close(release) })
				synctest.Wait()
				current := cl.snapshotConfig()
				if invalidate == "none" {
					require.Equal(t, int64(2), current.Client.Revision)
					require.Equal(t, "127.0.0.1:9998", current.Mappings[0].LocalAddr)
					require.Equal(t, "running", cl.Mappings()[0].Status)
					var sawUpdate, sawPush bool
					for len(replies) > 0 {
						switch msg := (<-replies).(type) {
						case *control.Update:
							sawUpdate = true
							require.Equal(t, int64(1), msg.Revision)
						case *control.PushAck:
							sawPush = true
							require.True(t, msg.OK)
							require.Equal(t, int64(2), msg.Revision)
						}
					}
					require.True(t, sawUpdate)
					require.True(t, sawPush)
				} else {
					require.Equal(t, int64(1), current.Client.Revision)
					require.Equal(t, "127.0.0.1:9999", current.Mappings[0].LocalAddr)
					require.Equal(t, int32(1), writes.Load(), "queued old-owner handlers were replayed after invalidation")
				}
			})
		})
	}
}

func TestExposureClientView(t *testing.T) {
	cfg := validTestClientConfig()
	cfg.Mappings = []config.Mapping{
		{ID: "m1", Type: "tcp", RemotePort: 23456, LocalAddr: "127.0.0.1:9001", Enabled: true},
	}
	path := filepath.Join(t.TempDir(), "client.yaml")
	require.NoError(t, config.SaveClient(path, cfg))
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	cl, err := New(cfg, nil)
	require.NoError(t, err)
	defer cl.Close()

	s := newSessionStub(transport.HY2)
	conn, peer := net.Pipe()
	s.control = conn
	done := make(chan error, 1)
	go func() { _, err := cl.runSession(context.Background(), s); done <- err }()
	defer func() {
		_ = peer.Close()
		_ = cl.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("control session did not stop")
		}
	}()

	msg, err := control.ReadMessage(peer)
	require.NoError(t, err)
	hello, ok := msg.(*control.Hello)
	require.True(t, ok)
	require.Contains(t, hello.Capabilities, control.CapabilityExposure)

	require.NoError(t, control.WriteMessage(peer, &control.HelloAck{
		Type: control.TypeHelloAck,
		OK:   true,
		Results: []control.Result{{
			ID:                  "m1",
			OK:                  true,
			AssignedPort:        23456,
			Exposure:            "public",
			UnavailableFamilies: []string{"ipv6"},
		}},
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, cl.WaitReady(ctx))

	views := cl.Mappings()
	require.Len(t, views, 1)
	require.Equal(t, view.ExposurePublic, views[0].Exposure)
	require.Equal(t, []string{"ipv6"}, views[0].UnavailableFamilies)

	require.NoError(t, control.WriteMessage(peer, &control.ExposureUpdate{
		Type:     control.TypeExposureUpdate,
		ID:       "m1",
		Exposure: "local",
	}))
	require.Eventually(t, func() bool {
		views := cl.Mappings()
		return len(views) == 1 && views[0].Exposure == view.ExposureLocal
	}, 2*time.Second, 10*time.Millisecond)
	require.Empty(t, cl.Mappings()[0].UnavailableFamilies)

	// Exposure is a read-only server read model: it must not touch the YAML or
	// advance the client revision.
	require.Equal(t, int64(0), cl.Revision())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)

	// An old server reports no exposure fields. The new client must keep the
	// value unknown instead of guessing public.
	oldServer := helloViews(cfg.Mappings, cfg.Mappings, []control.Result{{ID: "m1", OK: true, AssignedPort: 23456}})
	require.Empty(t, oldServer[0].Exposure)
	require.Empty(t, oldServer[0].UnavailableFamilies)

	// Update acks and pushes carry the same read model through control.Result.
	updated := mappingViews(cfg.Mappings, map[string]control.Result{
		"m1": {ID: "m1", OK: true, AssignedPort: 23456, Exposure: "local", UnavailableFamilies: []string{"ipv4"}},
	})
	require.Equal(t, view.ExposureLocal, updated[0].Exposure)
	require.Equal(t, []string{"ipv4"}, updated[0].UnavailableFamilies)

	pushed := pushViews(cfg.Mappings, nil, []control.Result{
		{ID: "m1", OK: true, Exposure: "public", UnavailableFamilies: []string{"ipv6"}},
	})
	require.Equal(t, view.ExposurePublic, pushed[0].Exposure)
	require.Equal(t, []string{"ipv6"}, pushed[0].UnavailableFamilies)
}

func TestHeartbeatControlQueueOverflowIsExplicitBoundedFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := validTestClientConfig()
		cfg.Mappings = []config.Mapping{{ID: "a", Type: "tcp", LocalAddr: "127.0.0.1:9", Enabled: true}}
		cl, err := New(cfg, nil)
		require.NoError(t, err)
		defer cl.Close()
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		cl.SetPersist(func(*config.ClientConfig) error { close(entered); <-release; return nil })
		s := newSessionStub(transport.HY2)
		conn, peer := net.Pipe()
		s.control = conn
		defer peer.Close()
		done := make(chan error, 1)
		go func() { _, err := cl.runSession(context.Background(), s); done <- err }()
		_, err = control.ReadMessage(peer)
		require.NoError(t, err)
		require.NoError(t, control.WriteMessage(peer, &control.HelloAck{Type: control.TypeHelloAck, OK: true, Results: []control.Result{{ID: "a", OK: true, AssignedPort: 23456}}}))
		<-entered
		for range 65 {
			if err := control.WriteMessage(peer, &control.UpdateAck{Type: control.TypeUpdateAck, Revision: 1}); err != nil {
				break
			}
		}
		synctest.Wait()
		select {
		case <-s.closed:
		default:
			t.Error("control admission silently exceeded its bounded queue")
		}
		releaseOnce.Do(func() { close(release) })
		err = <-done
		require.ErrorIs(t, err, transport.ErrQueueFull)
		require.False(t, transport.FallbackAllowed(err))
	})
}
