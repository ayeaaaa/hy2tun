package client

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/transport"
)

// Only time and the transport boundary are replaced; registration, heartbeat,
// persistence, ownership and the policy controller are the production code.
type policyClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []policyTimer
}
type policyTimer struct {
	at time.Time
	ch chan time.Time
}

func newPolicyClock() *policyClock    { return &policyClock{now: time.Now()} }
func (f *policyClock) Now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
func (f *policyClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 1)
	f.timers = append(f.timers, policyTimer{f.now.Add(d), ch})
	return ch
}
func (f *policyClock) advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	remaining := f.timers[:0]
	for _, timer := range f.timers {
		if !timer.at.After(f.now) {
			timer.ch <- f.now
		} else {
			remaining = append(remaining, timer)
		}
	}
	f.timers = remaining
	f.mu.Unlock()
	synctest.Wait()
}

type probeSession struct {
	*sessionStub
	probe func(context.Context) error
}

func (s *probeSession) Probe(ctx context.Context) (transport.ProbeInfo, error) {
	return transport.ProbeInfo{}, s.probe(ctx)
}
func (s *probeSession) OpenControl(context.Context) (net.Conn, error) {
	return nil, errors.New("ordinary Hello on isolated probe")
}

type policyHarness struct {
	clock         *policyClock
	client        *Client
	done          chan error
	mu            sync.Mutex
	kinds         []transport.Kind
	hellos        chan *control.Hello
	probes        []*probeSession
	probeErrors   []error
	gate          func(context.Context) error
	first         error
	formalError   string
	formalDrop    bool
	dialFailures  map[int]error
	ackGate       <-chan struct{}
	registrations []transport.Kind
}

func newPolicyHarness(t *testing.T, policy FailoverPolicy) *policyHarness {
	h := &policyHarness{clock: newPolicyClock(), done: make(chan error, 1), hellos: make(chan *control.Hello, 20), first: syscall.ECONNREFUSED}
	cfg := validTestClientConfig()
	cfg.Mode = config.ModeAuto
	cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Policy: policy, Clock: h.clock, Jitter: func(d time.Duration) time.Duration { return d }, Dial: h.dial})
	require.NoError(t, err)
	h.client = cl
	t.Cleanup(func() { require.NoError(t, cl.Close()); <-h.done; synctest.Wait() })
	go func() { h.done <- cl.Start(context.Background()) }()
	synctest.Wait()
	return h
}
func (h *policyHarness) dial(_ context.Context, kind transport.Kind, _ *config.ClientConfig) (transport.Session, error) {
	h.mu.Lock()
	h.kinds = append(h.kinds, kind)
	n := len(h.kinds)
	h.mu.Unlock()
	if n == 1 {
		return nil, h.first
	}
	if err := h.dialFailures[n]; err != nil {
		return nil, err
	}
	if kind == transport.HY2 {
		s := &probeSession{sessionStub: newSessionStub(kind)}
		s.probe = func(ctx context.Context) error {
			if h.gate != nil {
				return h.gate(ctx)
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			if len(h.probeErrors) == 0 {
				return nil
			}
			err := h.probeErrors[0]
			h.probeErrors = h.probeErrors[1:]
			return err
		}
		h.mu.Lock()
		h.probes = append(h.probes, s)
		h.mu.Unlock()
		// Candidate is the fourth HY2 session (three probes + candidate).
		// Its formal control stream is separate from the read-only Probe call.
		return &candidateSession{probeSession: s, open: func() net.Conn { return h.control(kind) }}, nil
	}
	s := newSessionStub(kind)
	s.control = h.control(kind)
	return s, nil
}

type candidateSession struct {
	*probeSession
	open func() net.Conn
}

func (s *candidateSession) OpenControl(context.Context) (net.Conn, error) {
	s.control = s.open()
	return s.control, nil
}
func (h *policyHarness) control(kind transport.Kind) net.Conn {
	h.mu.Lock()
	h.registrations = append(h.registrations, kind)
	h.mu.Unlock()
	conn, peer := net.Pipe()
	go func() {
		defer peer.Close()
		msg, err := control.ReadMessage(peer)
		if err != nil {
			return
		}
		h.hellos <- msg.(*control.Hello)
		if kind == transport.HY2 && h.formalDrop {
			return
		}
		ack := &control.HelloAck{Type: control.TypeHelloAck, OK: true}
		if kind == transport.HY2 && h.ackGate != nil {
			<-h.ackGate
		}
		if kind == transport.HY2 && h.formalError != "" {
			ack.OK = false
			ack.Error = h.formalError
		}
		if control.WriteMessage(peer, ack) != nil {
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
			}
		}
	}()
	return conn
}
func (h *policyHarness) calls() []transport.Kind {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]transport.Kind(nil), h.kinds...)
}
func (h *policyHarness) tick(n int) {
	for range n {
		h.clock.advance(10 * time.Second)
	}
}
func recoveryTestPolicy() FailoverPolicy {
	p := DefaultFailoverPolicy()
	p.ProbeInterval = 10 * time.Second
	return p
}

func TestAutoFallsBackOnNetworkFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newPolicyHarness(t, FailoverPolicy{})
		require.Equal(t, []transport.Kind{transport.HY2, transport.TCP}, h.calls())
		require.Equal(t, "tcp", h.client.TransportState().ActiveTransport)
		require.Zero(t, h.client.TransportState().SwitchCount)
		require.Equal(t, config.ModeAuto, h.client.snapshotConfig().Mode)
	})
}
func TestAutoDoesNotFallbackOnAuthOrTLS(t *testing.T) {
	for _, failure := range []error{transport.ErrAuthentication, transport.ErrCertificatePin, transport.ErrProtocol} {
		t.Run(failure.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := validTestClientConfig()
				cfg.Mode = config.ModeAuto
				var kinds []transport.Kind
				cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Dial: func(_ context.Context, k transport.Kind, _ *config.ClientConfig) (transport.Session, error) {
					kinds = append(kinds, k)
					return nil, errors.Join(failure, syscall.ECONNREFUSED)
				}})
				require.NoError(t, err)
				defer cl.Close()
				_, err = cl.runOnce(context.Background())
				require.ErrorIs(t, err, failure)
				require.Equal(t, []transport.Kind{transport.HY2}, kinds)
			})
		})
	}
}
func TestRecoveryRequiresThreeSuccessesAndHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newPolicyHarness(t, recoveryTestPolicy())
		h.tick(2)
		require.Equal(t, "tcp", h.client.TransportState().ActiveTransport)
		h.tick(1) // Three successes, but only 30 seconds held.
		require.Equal(t, "tcp", h.client.TransportState().ActiveTransport)
		h.tick(3)
		require.Equal(t, "hy2", h.client.TransportState().ActiveTransport)
		require.Equal(t, uint64(1), h.client.TransportState().SwitchCount)
		require.Equal(t, "hy2_recovered", h.client.TransportState().SwitchReason)
	})
}
func TestProbeFailureResetsRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := recoveryTestPolicy()
		p.MinTCPHold = time.Second
		h := newPolicyHarness(t, p)
		h.probeErrors = []error{nil, nil, syscall.ENETUNREACH, nil, nil, nil}
		h.tick(3)
		require.Equal(t, "tcp", h.client.TransportState().ActiveTransport)
		require.NotEmpty(t, h.client.TransportState().LastProbeError)
		h.tick(2)
		require.Equal(t, "tcp", h.client.TransportState().ActiveTransport)
		h.tick(1)
		require.Equal(t, "hy2", h.client.TransportState().ActiveTransport)
	})
}
func TestRecoveryProbeStaleEpochDiscarded(t *testing.T) {
	for _, change := range []string{"mode", "credentials", "close"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := recoveryTestPolicy()
				p.MinTCPHold = time.Second
				p.RecoverySuccesses = 1
				h := newPolicyHarness(t, p)
				release := make(chan struct{})
				h.gate = func(context.Context) error { <-release; return nil }
				h.tick(1)
				if change == "close" {
					require.NoError(t, h.client.Close())
				} else {
					h.client.configMu.Lock()
					next := cloneClientConfig(h.client.cfg)
					if change == "mode" {
						next.Mode = config.ModeTCP
					} else {
						next.Auth = "new-auth-password"
						next.Control.Token = "new-token"
						next.Credentials.Generation = 2
					}
					h.client.storeConfigLocked(next, h.client.udpTokenSnapshot(), h.client.mappingSnapshot())
					h.client.configMu.Unlock()
				}
				synctest.Wait()
				h.mu.Lock()
				pending := append([]*probeSession(nil), h.probes...)
				h.mu.Unlock()
				for _, probe := range pending {
					select {
					case <-probe.closed:
					default:
						t.Error("invalidation did not close pending probe root before its late return")
					}
				}
				close(release)
				synctest.Wait()
				require.NotEqual(t, "hy2", h.client.TransportState().ActiveTransport)
				require.Zero(t, h.client.TransportState().SwitchCount)
				h.mu.Lock()
				probes := append([]*probeSession(nil), h.probes...)
				h.mu.Unlock()
				for _, probe := range probes {
					select {
					case <-probe.closed:
					default:
						t.Fatal("stale probe leaked")
					}
				}
			})
		})
	}
}
func TestProbeDoesNotUseHello(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newPolicyHarness(t, recoveryTestPolicy())
		<-h.hellos // Initial TCP registration.
		writes := 0
		h.client.SetPersist(func(*config.ClientConfig) error { writes++; return nil })
		h.tick(2)
		require.Len(t, h.calls(), 4)
		require.Empty(t, h.hellos, "read-only probes sent ordinary Hello")
		require.Zero(t, writes)
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, probe := range h.probes {
			select {
			case <-probe.closed:
			default:
				t.Fatal("completed probe not closed")
			}
		}
	})
}
func TestAutomaticSwitchPreservesLatestMappings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := recoveryTestPolicy()
		p.MinTCPHold = time.Second
		p.RecoverySuccesses = 1
		h := newPolicyHarness(t, p)
		<-h.hellos
		release := make(chan struct{})
		probeCalls := 0
		h.gate = func(context.Context) error {
			probeCalls++
			if probeCalls == 2 {
				<-release
			}
			return nil
		}
		h.tick(1)
		h.client.configMu.Lock()
		next := cloneClientConfig(h.client.cfg)
		next.Client.Revision = 8
		next.Mappings = []config.Mapping{{ID: "latest", Type: "tcp", LocalAddr: "127.0.0.1:9876", RemotePort: 23456, Enabled: true}}
		h.client.storeConfigLocked(next, h.client.udpTokenSnapshot(), h.client.mappingSnapshot())
		h.client.configMu.Unlock()
		close(release)
		synctest.Wait()
		require.Equal(t, "hy2", h.client.TransportState().ActiveTransport)
		hello := <-h.hellos
		require.Equal(t, int64(8), hello.Revision)
		require.Equal(t, "127.0.0.1:9876", hello.Mappings[0].LocalAddr)
	})
}

func TestRecoveryCandidateFailureKeepsTCP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := recoveryTestPolicy()
		p.MinTCPHold = time.Second
		p.RecoverySuccesses = 1
		h := newPolicyHarness(t, p)
		h.probeErrors = []error{nil, io.EOF}
		h.tick(1)
		require.Equal(t, "tcp", h.client.TransportState().ActiveTransport)
		require.True(t, h.client.Status().Connected)
		require.Len(t, h.hellos, 1)
		require.Len(t, h.calls(), 4)
	})
}

func TestProbeTimeoutResetsRecoveryAndContinues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := recoveryTestPolicy()
		p.MinTCPHold = time.Second
		p.RecoverySuccesses = 1
		h := newPolicyHarness(t, p)
		h.gate = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
		h.tick(2) // Probe starts at 10s and times out at 20s.
		require.Equal(t, "timeout", h.client.TransportState().LastProbeError)
		h.gate = nil
		h.tick(1)
		require.Equal(t, "hy2", h.client.TransportState().ActiveTransport, "a timed-out probe must not disable recovery")
	})
}

func TestAutoEstablishedHY2HeartbeatFallsBackImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := validTestClientConfig()
		cfg.Mode = config.ModeAuto
		clock := newPolicyClock()
		var kinds []transport.Kind
		var peer net.Conn
		h := &policyHarness{hellos: make(chan *control.Hello, 4)}
		cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Clock: clock, Dial: func(_ context.Context, kind transport.Kind, _ *config.ClientConfig) (transport.Session, error) {
			kinds = append(kinds, kind)
			s := newSessionStub(kind)
			if kind == transport.TCP {
				s.control = h.control(kind)
				return s, nil
			}
			s.control, peer = net.Pipe()
			go func() {
				_, _ = control.ReadMessage(peer)
				_ = control.WriteMessage(peer, &control.HelloAck{Type: control.TypeHelloAck, OK: true})
				for {
					if _, err := control.ReadMessage(peer); err != nil {
						return
					}
				}
			}()
			return s, nil
		}})
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { done <- cl.Start(context.Background()) }()
		synctest.Wait()
		defer func() { cl.Close(); peer.Close(); <-done; synctest.Wait() }()
		require.Equal(t, "hy2", cl.TransportState().ActiveTransport)
		for range 3 {
			clock.advance(10 * time.Second)
		}
		require.Equal(t, []transport.Kind{transport.HY2, transport.TCP}, kinds)
		require.Equal(t, "tcp", cl.TransportState().ActiveTransport)
		require.Equal(t, uint64(1), cl.TransportState().SwitchCount)
	})
}

func TestAutoTCPFailureRetriesHY2ThenTCP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newPolicyHarness(t, recoveryTestPolicy())
		h.dialFailures = map[int]error{3: syscall.ECONNREFUSED}
		closeLiveConnection(nil, h.client.currentSession())
		synctest.Wait()
		h.clock.advance(time.Second)
		require.Equal(t, []transport.Kind{transport.HY2, transport.TCP, transport.HY2, transport.TCP}, h.calls())
		require.Equal(t, "tcp", h.client.TransportState().ActiveTransport)
		require.Zero(t, h.client.TransportState().SwitchCount, "same-kind reconnection is not a switch")
	})
}

func TestRecoveryDefaultProbeCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newPolicyHarness(t, FailoverPolicy{})
		h.tick(2)
		require.Len(t, h.calls(), 2)
		h.tick(1)
		require.Len(t, h.calls(), 3)
		h.tick(3)
		require.Len(t, h.calls(), 4)
		require.Equal(t, "tcp", h.client.TransportState().ActiveTransport)
		h.tick(3)
		require.Equal(t, "hy2", h.client.TransportState().ActiveTransport)
	})
}

func TestRecoveryCandidateStaleSuccessDoesNotHandoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := recoveryTestPolicy()
		p.MinTCPHold = time.Second
		p.RecoverySuccesses = 1
		h := newPolicyHarness(t, p)
		release := make(chan struct{})
		calls := 0
		h.gate = func(context.Context) error {
			calls++
			if calls == 2 {
				<-release
			}
			return nil
		}
		h.tick(1) // Successful isolated probe; candidate Probe remains in flight.
		h.client.configMu.Lock()
		next := cloneClientConfig(h.client.cfg)
		next.Auth = "replacement-password"
		next.Control.Token = "new-token"
		next.Credentials.Generation = 2
		h.client.storeConfigLocked(next, h.client.udpTokenSnapshot(), h.client.mappingSnapshot())
		h.client.configMu.Unlock()
		close(release)
		synctest.Wait()
		require.Equal(t, "tcp", h.client.TransportState().ActiveTransport)
		require.True(t, h.client.Status().Connected)
		h.mu.Lock()
		registered := append([]transport.Kind(nil), h.registrations...)
		probes := append([]*probeSession(nil), h.probes...)
		h.mu.Unlock()
		require.Equal(t, []transport.Kind{transport.TCP}, registered)
		for _, probe := range probes {
			select {
			case <-probe.closed:
			default:
				t.Fatal("stale candidate leaked")
			}
		}
	})
}

func TestRecoveryCandidateLateHelloAckCannotCommitNewGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := recoveryTestPolicy()
		p.MinTCPHold = time.Second
		p.RecoverySuccesses = 1
		h := newPolicyHarness(t, p)
		<-h.hellos
		release := make(chan struct{})
		h.ackGate = release
		h.tick(1)
		hello := <-h.hellos
		require.Equal(t, int64(1), hello.CredentialGeneration)
		h.dialFailures = map[int]error{5: errors.New("stop new-epoch attempt")}
		h.client.configMu.Lock()
		next := cloneClientConfig(h.client.cfg)
		next.Auth = "replacement-password"
		next.Control.Token = "new-token"
		next.Credentials.Generation = 2
		h.client.storeConfigLocked(next, h.client.udpTokenSnapshot(), h.client.mappingSnapshot())
		h.client.configMu.Unlock()
		close(release)
		synctest.Wait()
		require.Empty(t, h.client.TransportState().ActiveTransport)
		require.Zero(t, h.client.TransportState().SwitchCount, "an unaccepted candidate is not a successful transport switch")
		require.Equal(t, int64(2), h.client.snapshotConfig().Credentials.Generation)
	})
}

func TestAutoSetupBudgetIncludesDialAndHelloAck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := validTestClientConfig()
		cfg.Mode = config.ModeAuto
		clock := newPolicyClock()
		release := make(chan struct{})
		s := newSessionStub(transport.HY2)
		conn, peer := net.Pipe()
		s.control = conn
		defer peer.Close()
		var kinds []transport.Kind
		cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Clock: clock, Dial: func(_ context.Context, kind transport.Kind, _ *config.ClientConfig) (transport.Session, error) {
			kinds = append(kinds, kind)
			if kind == transport.HY2 {
				<-release
				return s, nil
			}
			return nil, errors.New("stop fallback")
		}})
		require.NoError(t, err)
		defer cl.Close()
		done := make(chan error, 1)
		go func() { _, err := cl.runOnce(context.Background()); done <- err }()
		synctest.Wait()
		clock.advance(7 * time.Second)
		close(release)
		synctest.Wait()
		_, err = control.ReadMessage(peer)
		require.NoError(t, err)
		synctest.Wait()
		clock.advance(3 * time.Second)
		select {
		case <-done:
		default:
			t.Fatal("HelloAck got a reset setup budget")
		}
		require.Equal(t, []transport.Kind{transport.HY2, transport.TCP}, kinds)
		select {
		case <-s.closed:
		default:
			t.Fatal("timed-out setup root leaked")
		}
	})
}

func TestAutoHelloAckSecurityFailureNeverTCP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := validTestClientConfig()
		cfg.Mode = config.ModeAuto
		h := &policyHarness{hellos: make(chan *control.Hello, 4), formalError: "unauthorized"}
		var kinds []transport.Kind
		cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Dial: func(_ context.Context, kind transport.Kind, _ *config.ClientConfig) (transport.Session, error) {
			kinds = append(kinds, kind)
			s := newSessionStub(kind)
			s.control = h.control(kind)
			return s, nil
		}})
		require.NoError(t, err)
		defer cl.Close()
		_, err = cl.runOnce(context.Background())
		require.ErrorIs(t, err, transport.ErrAuthentication)
		require.Equal(t, []transport.Kind{transport.HY2}, kinds)
		require.Empty(t, cl.TransportState().ActiveTransport)
	})
}

func TestAutoDialTimeoutFallsBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := validTestClientConfig()
		cfg.Mode = config.ModeAuto
		clock := newPolicyClock()
		h := &policyHarness{hellos: make(chan *control.Hello, 4)}
		var kinds []transport.Kind
		cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Clock: clock, Dial: func(ctx context.Context, kind transport.Kind, _ *config.ClientConfig) (transport.Session, error) {
			kinds = append(kinds, kind)
			if kind == transport.HY2 {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			s := newSessionStub(kind)
			s.control = h.control(kind)
			return s, nil
		}})
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { done <- cl.Start(context.Background()) }()
		synctest.Wait()
		defer func() { cl.Close(); <-done; synctest.Wait() }()
		clock.advance(10 * time.Second)
		require.Equal(t, []transport.Kind{transport.HY2, transport.TCP}, kinds)
		require.Equal(t, "tcp", cl.TransportState().ActiveTransport)
	})
}

func TestAutoOldEpochFailureDoesNotFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := validTestClientConfig()
		cfg.Mode = config.ModeAuto
		clock := newPolicyClock()
		s := newSessionStub(transport.HY2)
		conn, peer := net.Pipe()
		s.control = conn
		defer peer.Close()
		var kinds []transport.Kind
		cl, err := NewWithRuntime(cfg, nil, RuntimeOptions{Clock: clock, Dial: func(_ context.Context, kind transport.Kind, _ *config.ClientConfig) (transport.Session, error) {
			kinds = append(kinds, kind)
			if len(kinds) == 1 {
				return s, nil
			}
			return nil, errors.New("stop retry")
		}})
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() { done <- cl.Start(context.Background()) }()
		_, err = control.ReadMessage(peer)
		require.NoError(t, err)
		require.NoError(t, control.WriteMessage(peer, &control.HelloAck{Type: control.TypeHelloAck, OK: true}))
		synctest.Wait()
		defer func() { cl.Close(); <-done; synctest.Wait() }()
		cl.configMu.Lock()
		next := cloneClientConfig(cl.cfg)
		next.Auth = "replacement-password"
		next.Control.Token = "new-token"
		next.Credentials.Generation = 2
		cl.storeConfigLocked(next, cl.udpTokenSnapshot(), cl.mappingSnapshot())
		cl.configMu.Unlock()
		peer.Close()
		synctest.Wait()
		require.Equal(t, []transport.Kind{transport.HY2, transport.HY2}, kinds, "an old epoch's EOF cannot choose TCP for the new credentials")
	})
}

func TestRecoveryFormalNetworkFailureRestoresTCP(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := recoveryTestPolicy()
		p.MinTCPHold = time.Second
		p.RecoverySuccesses = 1
		h := newPolicyHarness(t, p)
		h.formalDrop = true
		h.tick(1)
		require.Equal(t, []transport.Kind{transport.HY2, transport.TCP, transport.HY2, transport.HY2, transport.TCP}, h.calls())
		require.Equal(t, "tcp", h.client.TransportState().ActiveTransport)
		require.Zero(t, h.client.TransportState().SwitchCount)
	})
}

func TestRecoveryFormalSecurityFailureDoesNotDowngrade(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := recoveryTestPolicy()
		p.MinTCPHold = time.Second
		p.RecoverySuccesses = 1
		h := newPolicyHarness(t, p)
		h.formalError = "unauthorized"
		h.tick(1)
		require.Equal(t, []transport.Kind{transport.HY2, transport.TCP, transport.HY2, transport.HY2}, h.calls())
		require.Empty(t, h.client.TransportState().ActiveTransport)
		require.Zero(t, h.client.TransportState().SwitchCount)
	})
}
