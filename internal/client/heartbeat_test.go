package client

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/transport"
)

func heartbeatFixture(t *testing.T) (*Client, *policyClock, *sessionStub, net.Conn, <-chan error) {
	clock := newPolicyClock()
	cl, err := NewWithRuntime(validTestClientConfig(), nil, RuntimeOptions{Clock: clock})
	require.NoError(t, err)
	s := newSessionStub(transport.HY2)
	conn, peer := net.Pipe()
	s.control = conn
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() { defer close(finished); _, err := cl.runSession(context.Background(), s); done <- err }()
	_, err = control.ReadMessage(peer)
	require.NoError(t, err)
	require.NoError(t, control.WriteMessage(peer, &control.HelloAck{Type: control.TypeHelloAck, OK: true}))
	synctest.Wait()
	t.Cleanup(func() { cl.Close(); peer.Close(); <-finished; synctest.Wait() })
	return cl, clock, s, peer, done
}
func readHeartbeat(t *testing.T, peer net.Conn) *control.Ping {
	msg, err := control.ReadMessage(peer)
	require.NoError(t, err)
	ping, ok := msg.(*control.Ping)
	require.True(t, ok)
	return ping
}
func TestHeartbeatTimeoutClosesSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, clock, s, peer, _ := heartbeatFixture(t)
		for range 2 {
			clock.advance(10 * time.Second)
			readHeartbeat(t, peer)
			synctest.Wait()
		}
		clock.advance(10 * time.Second)
		select {
		case <-s.closed:
		default:
			t.Fatal("30 seconds without Pong did not close root")
		}
	})
}
func TestHeartbeatPongRenewsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, clock, s, peer, _ := heartbeatFixture(t)
		for range 5 {
			clock.advance(10 * time.Second)
			ping := readHeartbeat(t, peer)
			require.NoError(t, control.WriteMessage(peer, &control.Pong{Type: control.TypePong, TS: ping.TS}))
			synctest.Wait()
			select {
			case <-s.closed:
				t.Fatal("correct Pong did not renew deadline")
			default:
			}
		}
	})
}
func TestHeartbeatLatePongIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl, clock, s, peer, _ := heartbeatFixture(t)
		clock.advance(10 * time.Second)
		first := readHeartbeat(t, peer)
		synctest.Wait()
		clock.advance(10 * time.Second)
		second := readHeartbeat(t, peer)
		synctest.Wait()
		require.Greater(t, second.TS, first.TS)
		require.NoError(t, control.WriteMessage(peer, &control.Pong{Type: control.TypePong, TS: first.TS}))
		other, otherPeer := net.Pipe()
		defer other.Close()
		defer otherPeer.Close()
		cl.handlePong(other, second.TS) // Correct TS on an old/different owner is also ignored.
		synctest.Wait()
		clock.advance(10 * time.Second)
		select {
		case <-s.closed:
		default:
			t.Fatal("stale Pong prolonged session")
		}
	})
}

func TestHeartbeatNoFallbackInForcedMode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := validTestClientConfig()
		cfg.Mode = config.ModeTCP
		clock := newPolicyClock()
		calls := 0
		cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Clock: clock, Dial: func(context.Context, transport.Kind, *config.ClientConfig) (transport.Session, error) {
			calls++
			return nil, context.DeadlineExceeded
		}})
		require.NoError(t, err)
		defer cl.Close()
		_, _ = cl.runOnce(context.Background())
		require.Equal(t, 1, calls)
	})
}

func TestHeartbeatHelloAckAdmissionDoesNotWaitForConfigPersistence(t *testing.T) {
	cl, err := New(validTestClientConfig(), nil)
	require.NoError(t, err)
	s := newSessionStub(transport.HY2)
	conn, peer := net.Pipe()
	s.control = conn
	done := make(chan error, 1)
	setup, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go func() { _, err := cl.runSessionWithSetup(context.Background(), setup, s, 1); done <- err }()
	_, err = control.ReadMessage(peer)
	require.NoError(t, err)
	cl.configMu.Lock() // A concurrent local persistence operation.
	require.NoError(t, control.WriteMessage(peer, &control.HelloAck{Type: control.TypeHelloAck, OK: true}))
	<-setup.Done()
	cl.mu.Lock()
	accepted := cl.connected
	cl.mu.Unlock()
	closed := false
	select {
	case <-s.closed:
		closed = true
	default:
	}
	cl.configMu.Unlock()
	cl.Close()
	peer.Close()
	<-done
	require.True(t, accepted, "wire acceptance must precede waiting for local config persistence")
	require.False(t, closed, "setup timer still owned a wire-accepted root")
}

func TestHeartbeatProtocolErrorWinsRacingTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl, _, _, peer, done := heartbeatFixture(t)
		// Model a timeout already reported when the reader observes a protocol
		// violation. A timeout must not erase the deny classification.
		cl.mu.Lock()
		cl.heartbeat.failed <- context.DeadlineExceeded
		cl.mu.Unlock()
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], control.MaxFrameSize+1)
		_, err := peer.Write(header[:])
		require.NoError(t, err)
		synctest.Wait()
		err = <-done
		require.ErrorIs(t, err, transport.ErrProtocol)
		require.False(t, transport.FallbackAllowed(err))
	})
}

func TestAutomaticSwitchStateWaitsForNewHelloAck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl, _, _, _, _ := heartbeatFixture(t)
		require.Equal(t, "hy2", cl.TransportState().ActiveTransport)
		replacement := newSessionStub(transport.TCP)
		owner, peer := net.Pipe()
		defer peer.Close()
		require.True(t, cl.setCurrent(replacement, owner))
		require.Empty(t, cl.TransportState().ActiveTransport, "an installed but unaccepted owner is not active")
		require.False(t, cl.Status().Connected)
	})
}

func TestHeartbeatTimeoutWhilePingWriteBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, clock, session, _, _ := heartbeatFixture(t)
		// The peer never reads Ping; timeout still uses the injected Clock,
		// rather than waiting for a real socket deadline or writer completion.
		for range 3 {
			clock.advance(10 * time.Second)
		}
		select {
		case <-session.closed:
		default:
			t.Fatal("blocked Ping writer disabled the 30-second heartbeat timeout")
		}
	})
}
