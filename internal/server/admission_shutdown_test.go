package server

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/fatedier/yamux"
	"github.com/stretchr/testify/require"
	"hy2tun/internal/control"
	"hy2tun/internal/transport"
)

type registrationCloseGate struct {
	net.Conn
	entered, release chan struct{}
	once             sync.Once
}

func (c *registrationCloseGate) Close() error {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.Conn.Close()
}

// Real TLS/authenticated server peer with SYNs queued on the real mux backlog.
// Only physical raw Close is gated; no admission/manager behavior is mocked.
func remotelyClosedRegistrationPeer(t *testing.T, s *Server, tlsConfig *tls.Config) (*transport.TCPPeer, func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type accepted struct {
		peer *transport.TCPPeer
		gate *registrationCloseGate
		err  error
	}
	ready := make(chan accepted, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			ready <- accepted{err: err}
			return
		}
		gate := &registrationCloseGate{Conn: raw, entered: make(chan struct{}), release: make(chan struct{})}
		peer, err := transport.AcceptTCP(ctx, gate, s.tcpServerOptions(tlsConfig))
		ready <- accepted{peer: peer, gate: gate, err: err}
	}()
	raw, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	require.NoError(t, err)
	client := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.SetDeadline(time.Now().Add(3*time.Second)))
	require.NoError(t, client.HandshakeContext(ctx))
	body, err := json.Marshal(transport.Greeting{Magic: "hy2tun-tcp", Version: 1, Auth: "pw", ControlToken: s.currentCredentials().ControlToken, ClientID: "queued"})
	require.NoError(t, err)
	_, err = client.Write(binary.BigEndian.AppendUint32(nil, uint32(len(body))))
	require.NoError(t, err)
	_, err = client.Write(body)
	require.NoError(t, err)
	var prefix [4]byte
	_, err = io.ReadFull(client, prefix[:])
	require.NoError(t, err)
	size := binary.BigEndian.Uint32(prefix[:])
	require.LessOrEqual(t, size, uint32(65536))
	ackBody := make([]byte, size)
	_, err = io.ReadFull(client, ackBody)
	require.NoError(t, err)
	var ack transport.GreetingAck
	require.NoError(t, json.Unmarshal(ackBody, &ack))
	require.True(t, ack.OK)
	acceptedPeer := <-ready
	require.NoError(t, acceptedPeer.err)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(acceptedPeer.gate.release) }) }
	t.Cleanup(func() { release(); _ = acceptedPeer.peer.Close() })
	require.NoError(t, client.SetDeadline(time.Time{}))
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.StreamOpenTimeout = 0
	cfg.LogOutput = io.Discard
	mux, err := yamux.Client(client, cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mux.Close() })
	for i := 0; i < 32; i++ {
		_, err := mux.OpenStream()
		require.NoError(t, err)
	}
	// The ping round trip proves the server processed the preceding SYNs.
	_, err = mux.Ping()
	require.NoError(t, err)
	require.NoError(t, mux.Close())
	select {
	case <-acceptedPeer.gate.entered:
	case <-time.After(time.Second):
		t.Fatal("server mux never entered physical teardown")
	}
	return acceptedPeer.peer, release
}

func TestTCPPeerClosedAdmissionDoesNotHoldManagerDuringQueuedShutdown(t *testing.T) {
	s, tlsConfig := tcpTestServer(t)
	peer, release := remotelyClosedRegistrationPeer(t, s, tlsConfig)
	defer release()
	principal := peer.Principal()
	for attempt := 0; attempt < 32; attempt++ {
		admitted := make(chan error, 1)
		go func() {
			sess := &clientSession{id: "queued", transport: transport.TCP, tcpPeer: peer, ready: make(chan struct{}), done: make(chan struct{})}
			_, err := s.registerControl(sess, &control.Hello{ClientID: "queued", Token: s.currentCredentials().ControlToken},
				sessionMeta{Kind: transport.TCP, Principal: &principal, Peer: peer}, time.Now().Add(time.Second), 1, false)
			admitted <- err
		}()
		blocked, pendingAdmission := false, false
		select {
		case err := <-admitted:
			if !errors.Is(err, net.ErrClosed) {
				t.Errorf("closed admission performed I/O classification: %v", err)
			}
		case <-time.After(100 * time.Millisecond):
			blocked = true
			pendingAdmission = true
			t.Error("closed admission waited for remote mux teardown")
		}
		managerDone, settingsDone := make(chan struct{}), make(chan struct{})
		go func() { s.mappings.clientCount(); close(managerDone) }()
		go func() { s.settingsMu.Lock(); s.settingsMu.Unlock(); close(settingsDone) }()
		for _, operation := range []struct {
			name string
			done <-chan struct{}
		}{{"manager", managerDone}, {"settings writer", settingsDone}} {
			select {
			case <-operation.done:
			case <-time.After(100 * time.Millisecond):
				t.Errorf("unrelated %s access blocked by rejected admission", operation.name)
				blocked = true
			}
		}
		if blocked {
			release()
			for _, done := range []<-chan struct{}{managerDone, settingsDone} {
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("shared lock did not recover after teardown released")
				}
			}
			if pendingAdmission {
				select {
				case <-admitted:
				case <-time.After(time.Second):
					t.Fatal("admission did not recover")
				}
			}
			return
		}
	}
	require.Empty(t, s.mappings.sessionSnapshot(), "closed root must not publish ownership")
}
