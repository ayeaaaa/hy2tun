package client

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"hy2tun/internal/config"
	"hy2tun/internal/transport"
	"hy2tun/internal/view"
)

type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type FailoverPolicy struct {
	ConnectTimeout, HeartbeatInterval, HeartbeatTimeout, ProbeInterval, MinTCPHold time.Duration
	RecoverySuccesses                                                              int
}

func DefaultFailoverPolicy() FailoverPolicy {
	f := config.FailoverConfig{}.Effective()
	return FailoverPolicy{
		ConnectTimeout:    time.Duration(f.ConnectTimeout),
		HeartbeatInterval: time.Duration(f.HeartbeatInterval),
		HeartbeatTimeout:  time.Duration(f.HeartbeatTimeout),
		ProbeInterval:     time.Duration(f.ProbeInterval),
		MinTCPHold:        time.Duration(f.MinTCPHold),
		RecoverySuccesses: f.RecoverySuccesses,
	}
}

// policyFromConfig applies one server's optional failover settings. A nil
// section keeps the conservative defaults; RuntimeOptions.Policy remains the
// explicit test/runtime override.
func policyFromConfig(cfg *config.ClientConfig) FailoverPolicy {
	if cfg == nil || cfg.Failover == nil {
		return DefaultFailoverPolicy()
	}
	f := cfg.Failover.Effective()
	return FailoverPolicy{
		ConnectTimeout:    time.Duration(f.ConnectTimeout),
		HeartbeatInterval: time.Duration(f.HeartbeatInterval),
		HeartbeatTimeout:  time.Duration(f.HeartbeatTimeout),
		ProbeInterval:     time.Duration(f.ProbeInterval),
		MinTCPHold:        time.Duration(f.MinTCPHold),
		RecoverySuccesses: f.RecoverySuccesses,
	}
}

type RuntimeOptions struct {
	Policy FailoverPolicy
	Clock  Clock
	Jitter func(time.Duration) time.Duration
	Dial   func(context.Context, transport.Kind, *config.ClientConfig) (transport.Session, error)
	// Asynchronous injected dials must use this seam and forward the actual
	// native/late-cleanup receipt. Dial alone is a synchronous boundary.
	DialWithCompletion func(context.Context, transport.Kind, *config.ClientConfig) (transport.Session, <-chan struct{}, error)
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

func (o RuntimeOptions) defaults() RuntimeOptions {
	d := DefaultFailoverPolicy()
	if o.Policy.ConnectTimeout <= 0 {
		o.Policy.ConnectTimeout = d.ConnectTimeout
	}
	if o.Policy.HeartbeatInterval <= 0 {
		o.Policy.HeartbeatInterval = d.HeartbeatInterval
	}
	if o.Policy.HeartbeatTimeout <= 0 {
		o.Policy.HeartbeatTimeout = d.HeartbeatTimeout
	}
	if o.Policy.ProbeInterval <= 0 {
		o.Policy.ProbeInterval = d.ProbeInterval
	}
	if o.Policy.MinTCPHold <= 0 {
		o.Policy.MinTCPHold = d.MinTCPHold
	}
	if o.Policy.RecoverySuccesses <= 0 {
		o.Policy.RecoverySuccesses = d.RecoverySuccesses
	}
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.Jitter == nil {
		o.Jitter = func(d time.Duration) time.Duration { return time.Duration(float64(d) * (0.8 + rand.Float64()*0.4)) }
	}
	return o
}

// NewWithRuntime injects policy time and the dial boundary without wrapping a
// Session (and therefore without hiding optional native completion receipts).
func NewWithRuntime(cfg *config.ClientConfig, log *slog.Logger, opts RuntimeOptions) (*Client, error) {
	return newClientWithRuntime(cfg, log, false, opts)
}

func (c *Client) TransportState() view.TransportState {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.transportState
	state.Mode = c.cfg.Mode.Effective()
	return state
}

// configMu -> mu is the shared admission boundary. Mapping edits are purposely
// excluded: a successful recovery registers the latest revision/targets.
func sameTransportConfig(a, b *config.ClientConfig) bool {
	return a.Mode.Effective() == b.Mode.Effective() && sameTransportIdentity(a, b)
}

// sameTransportIdentity compares every transport identity field except the
// effective mode. A mode-only update can retain an owner whose kind remains
// allowed, while any credential/endpoint/TLS change still invalidates it.
func sameTransportIdentity(a, b *config.ClientConfig) bool {
	return a.Server == b.Server &&
		a.Auth == b.Auth && a.Control.Token == b.Control.Token && a.Client.ID == b.Client.ID &&
		a.Credentials.Generation == b.Credentials.Generation && a.TLS == b.TLS &&
		a.Bandwidth == b.Bandwidth && a.Obfs == b.Obfs
}

// invalidateTransportLocked requires mu (and configMu when publishing config).
// Task 8 can publish via storeConfigLocked then forceReconnect; queued retries,
// probes and unaccepted candidates are cancelled, not allowed to commit late.
func (c *Client) invalidateTransportLocked() {
	c.transportEpoch++
	c.fallbackNetworkEligible = false
	if c.transportWork != nil {
		c.transportWork.cancel()
	}
	select {
	case c.transportChanged <- struct{}{}:
	default:
	}
}

type attemptKey struct{}
type transportAttempt struct {
	cfg         *config.ClientConfig
	epoch       uint64
	kind        transport.Kind
	reason      string
	setup       context.Context
	cancel      context.CancelFunc
	session     transport.Session
	ownershipMu sync.Mutex
	admitted    bool
	stopClose   func() bool
}
type sessionRunKey struct{}
type sessionRun struct{ candidate *transportAttempt }

// Temporary roots remain cancellation-owned even before OpenControl, and while
// a Probe implementation is returning late. Admission arbitrates with that
// callback before detaching it; Close never implies native data-work drainage.
func (a *transportAttempt) ownSession(session transport.Session) {
	a.session = session
	a.stopClose = context.AfterFunc(a.setup, func() {
		a.ownershipMu.Lock()
		closeRoot := !a.admitted
		a.ownershipMu.Unlock()
		if closeRoot {
			_ = session.Close()
		}
	})
}
func (a *transportAttempt) admit() error {
	a.ownershipMu.Lock()
	defer a.ownershipMu.Unlock()
	if err := setupContextError(a.setup); err != nil {
		return err
	}
	a.admitted = true
	if a.stopClose != nil {
		a.stopClose()
	}
	return nil
}

// A Clock-driven timeout also exposes a real socket deadline. Err retains
// DeadlineExceeded rather than turning a network timeout into denied cancellation.
type policyDeadline struct {
	context.Context
	deadline time.Time
	done     <-chan struct{}
}

func (p policyDeadline) Deadline() (time.Time, bool) { return p.deadline, true }

// A distinct Done prevents context's internal cancelCtx fast path from copying
// the backing context's Canceled Err into children instead of our timeout Err.
func (p policyDeadline) Done() <-chan struct{} { return p.done }
func (p policyDeadline) Err() error {
	if p.Context.Err() != nil && errors.Is(context.Cause(p.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return p.Context.Err()
}
func (c *Client) setupTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	d := c.runtime.Policy.ConnectTimeout
	deadline := time.Now().Add(d)
	if prior, ok := parent.Deadline(); ok && prior.Before(deadline) {
		deadline = prior
		d = time.Until(prior)
	}
	base, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	timer := c.runtime.Clock.After(d)
	go func() {
		defer close(done)
		select {
		case <-timer:
			cancel(context.DeadlineExceeded)
		case <-base.Done():
		}
	}()
	return policyDeadline{base, deadline, done}, func() { cancel(context.Canceled) }
}

func (c *Client) newAttempt(ctx context.Context, kind transport.Kind, reason string) *transportAttempt {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	setup, cancel := c.setupTimeout(ctx)
	a := &transportAttempt{cfg: cloneClientConfig(c.cfg), kind: kind, reason: reason, cancel: cancel}
	switch c.cfg.Mode.Effective() {
	case config.ModeTCP:
		a.kind = transport.TCP
	case config.ModeHY2:
		a.kind = transport.HY2
	}
	if (reason == "probe" || reason == "hy2_recovered") && c.cfg.Mode.Effective() != config.ModeAuto {
		cancel()
	}
	c.mu.Lock()
	a.epoch = c.transportEpoch
	c.transportWork = a
	if c.isClosed() {
		cancel()
	}
	c.mu.Unlock()
	a.setup = context.WithValue(setup, attemptKey{}, a)
	return a
}
func (c *Client) finishAttempt(a *transportAttempt) {
	c.mu.Lock()
	if c.transportWork == a {
		c.transportWork = nil
	}
	c.mu.Unlock()
}
func (c *Client) discardAttempt(a *transportAttempt) {
	if a == nil {
		return
	}
	a.cancel()
	if a.session != nil {
		_ = a.session.Close()
	}
	c.finishAttempt(a)
}
func (c *Client) attemptCurrentLocked(a *transportAttempt) bool {
	return !c.isClosed() && a.epoch == c.transportEpoch && sameTransportConfig(a.cfg, c.cfg) && a.setup.Err() == nil
}

func (c *Client) runAttempt(ctx context.Context, a *transportAttempt) (bool, *transportAttempt, error) {
	defer a.cancel()
	defer c.finishAttempt(a)
	if a.session == nil {
		s, err := c.dialSession(a.setup, a.kind, a.cfg)
		if err != nil {
			c.mu.Lock()
			stale := a.epoch != c.transportEpoch
			c.mu.Unlock()
			if stale && ctx.Err() == nil && !c.isClosed() {
				return false, nil, nil
			}
			return false, nil, err
		}
		a.ownSession(s)
	}
	run := &sessionRun{}
	connected, err := c.runSessionWithSetup(context.WithValue(ctx, sessionRunKey{}, run), a.setup, a.session, a.cfg.Credentials.Generation)
	c.mu.Lock()
	stale := a.epoch != c.transportEpoch
	c.mu.Unlock()
	if stale && ctx.Err() == nil && !c.isClosed() {
		c.discardAttempt(run.candidate)
		return connected, nil, nil
	}
	return connected, run.candidate, err
}

func (c *Client) runOnce(ctx context.Context) (bool, error) {
	kind := transport.HY2
	if c.snapshotConfig().Mode.Effective() == config.ModeTCP {
		kind = transport.TCP
	}
	a := c.newAttempt(ctx, kind, "connected")
	for {
		connected, candidate, err := c.runAttempt(ctx, a)
		if candidate != nil {
			a = candidate
			continue
		}
		if ctx.Err() != nil || c.isClosed() {
			return connected, err
		}
		if c.snapshotConfig().Mode.Effective() != config.ModeAuto {
			return connected, err
		}
		c.mu.Lock()
		busy := setupBusyOnly(err)
		if err != nil && !busy && !transport.FallbackAllowed(errors.Join(err, context.DeadlineExceeded)) {
			c.fallbackNetworkEligible = false
		}
		eligible := transport.FallbackAllowed(err)
		if a.kind == transport.HY2 && eligible {
			c.fallbackNetworkEligible = true
		}
		allowed := eligible || (busy && c.fallbackNetworkEligible)
		c.mu.Unlock()
		if a.kind == transport.HY2 && allowed {
			reason := "hy2_unavailable"
			if connected {
				reason = "hy2_inactive"
			}
			a = c.newAttempt(ctx, transport.TCP, reason)
			continue
		}
		return connected, err
	}
}

func (c *Client) transportAcceptedLocked(session transport.Session, setup context.Context) {
	kind, reason := string(session.Kind()), "connected"
	if session.Kind() == transport.HY2 {
		c.fallbackNetworkEligible = false
	}
	if a, ok := setup.Value(attemptKey{}).(*transportAttempt); ok {
		reason = a.reason
		if c.transportWork == a {
			c.transportWork = nil
		}
		a.cancel() // The shared setup timer no longer owns accepted lifetime.
	}
	now := c.runtime.Clock.Now()
	if c.lastTransport != "" && c.lastTransport != kind {
		c.transportState.SwitchCount++
		c.transportState.LastSwitchAt = now
		c.transportState.SwitchReason = reason
	} else if c.lastTransport == "" {
		c.transportState.SwitchReason = reason
	}
	c.lastTransport = kind
	c.transportState.ActiveTransport = kind
	c.activeSince = now
}

func (c *Client) recoveryCurrent(a *transportAttempt, session transport.Session, owner net.Conn) bool {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attemptCurrentLocked(a) && c.cfg.Mode.Effective() == config.ModeAuto && c.session == session && c.controlConn == owner && c.connected
}

// probePolicyActive reports whether the persistent TCP policy loop should
// probe. Mode changes wake the loop through modeChanged.
func (c *Client) probePolicyActive() bool {
	return c.snapshotConfig().Mode.Effective() == config.ModeAuto
}

// installProbeWork records the one outstanding probe/candidate. A mode update
// that disables probing cancels this attempt without cancelling an allowed
// active owner.
func (c *Client) installProbeWork(a *transportAttempt) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed() {
		return false
	}
	c.probeWork = a
	return true
}

func (c *Client) clearProbeWork(a *transportAttempt) {
	c.mu.Lock()
	if c.probeWork == a {
		c.probeWork = nil
	}
	c.mu.Unlock()
}

// recoveryOwnerLive reports whether the policy loop still belongs to the same
// wire-accepted owner. It deliberately ignores the epoch: a mode-only change
// advances the epoch but retains an allowed owner, and the loop refreshes its
// epoch from transportSnapshot instead of exiting.
func (c *Client) recoveryOwnerLive(session transport.Session, owner net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.isClosed() && c.session == session && c.controlConn == owner && c.connected
}

// recoveryIdentityCurrent distinguishes a mode-only change from a credential,
// endpoint or TLS change that must stop the old policy loop.
func (c *Client) recoveryIdentityCurrent(cfg *config.ClientConfig) bool {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	return sameTransportIdentity(cfg, c.cfg)
}

func (c *Client) transportSnapshot() (uint64, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.transportEpoch, c.activeSince
}

func (c *Client) probeAttempt(a *transportAttempt) error {
	s, err := c.dialSession(a.setup, transport.HY2, a.cfg)
	if err != nil {
		return err
	}
	a.ownSession(s)
	err = c.probeSetup(a.setup, s)
	if err == nil {
		err = a.setup.Err()
	}
	return err
}

func (c *Client) recordProbe(a *transportAttempt, session transport.Session, owner net.Conn, err error) bool {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed() || a.epoch != c.transportEpoch || !sameTransportConfig(a.cfg, c.cfg) || errors.Is(a.setup.Err(), context.Canceled) || c.cfg.Mode.Effective() != config.ModeAuto || c.session != session || c.controlConn != owner || !c.connected {
		return false
	}
	c.transportState.LastProbeAt = c.runtime.Clock.Now()
	c.transportState.LastProbeError = ""
	if err != nil {
		c.transportState.LastProbeError = probeErrorCode(err)
		if !setupBusyOnly(err) {
			c.fallbackNetworkEligible = transport.FallbackAllowed(err)
		}
	}
	return true
}

// Never expose error strings that could contain credentials or endpoint data.
func probeErrorCode(err error) string {
	switch {
	case errors.Is(err, transport.ErrAuthentication):
		return "authentication"
	case errors.Is(err, transport.ErrCertificatePin):
		return "certificate_pin"
	case errors.Is(err, transport.ErrProtocol):
		return "protocol"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case transport.FallbackAllowed(err):
		return "network"
	default:
		return "probe_failed"
	}
}

// recoveryLoop owns one isolated HY2 probe at a time. It persists for the TCP
// owner's lifetime and probes only while the effective mode is auto; a mode
// update that retains this owner stops any in-flight probe/candidate through
// probeWork and wakes the loop without replacing the owner.
func (c *Client) recoveryLoop(lifetime, ctx context.Context, session transport.Session, owner net.Conn) *transportAttempt {
	successes := 0
	for {
		// A mode change advances the epoch while retaining an allowed owner;
		// refresh here so subsequent attempts use the current identity.
		epoch, heldAt := c.transportSnapshot()
		if !c.probePolicyActive() {
			successes = 0
			select {
			case <-ctx.Done():
				return nil
			case <-c.modeChanged:
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-c.modeChanged:
			successes = 0
			continue
		case <-c.runtime.Clock.After(c.runtime.Jitter(c.runtime.Policy.ProbeInterval)):
		}
		if !c.probePolicyActive() {
			continue
		}
		a := c.newAttempt(ctx, transport.HY2, "probe")
		if a.epoch != epoch || !c.installProbeWork(a) {
			c.discardAttempt(a)
			if !c.recoveryOwnerLive(session, owner) || !c.recoveryIdentityCurrent(a.cfg) {
				return nil
			}
			successes = 0
			continue
		}
		if !c.probePolicyActive() {
			c.clearProbeWork(a)
			c.discardAttempt(a)
			successes = 0
			continue
		}
		err := c.probeAttempt(a)
		valid := c.recordProbe(a, session, owner, err)
		c.clearProbeWork(a)
		c.discardAttempt(a) // Success and failure both close every isolated probe.
		if !valid {
			if !c.recoveryOwnerLive(session, owner) || !c.recoveryIdentityCurrent(a.cfg) {
				return nil
			}
			successes = 0
			continue
		}
		if err != nil {
			successes = 0
			continue
		}
		successes++
		if successes < c.runtime.Policy.RecoverySuccesses || c.runtime.Clock.Now().Sub(heldAt) < c.runtime.Policy.MinTCPHold {
			continue
		}
		candidate := c.newAttempt(lifetime, transport.HY2, "hy2_recovered")
		stop := context.AfterFunc(ctx, candidate.cancel)
		if candidate.epoch != epoch || !c.installProbeWork(candidate) {
			stop()
			c.discardAttempt(candidate)
			if !c.recoveryOwnerLive(session, owner) || !c.recoveryIdentityCurrent(candidate.cfg) {
				return nil
			}
			successes = 0
			continue
		}
		if !c.probePolicyActive() {
			stop()
			c.clearProbeWork(candidate)
			c.discardAttempt(candidate)
			successes = 0
			continue
		}
		err = c.probeAttempt(candidate)
		valid = c.recordProbe(candidate, session, owner, err)
		c.clearProbeWork(candidate)
		if !stop() || !valid || err != nil {
			c.discardAttempt(candidate)
			if ctx.Err() != nil || !c.recoveryOwnerLive(session, owner) || !c.recoveryIdentityCurrent(candidate.cfg) {
				return nil
			}
			successes = 0
			continue
		}
		if !c.recoveryCurrent(candidate, session, owner) {
			c.discardAttempt(candidate)
			if !c.recoveryOwnerLive(session, owner) || !c.recoveryIdentityCurrent(candidate.cfg) {
				return nil
			}
			successes = 0
			continue
		}
		return candidate
	}
}

// The old control reader and recovery worker are joined before a new formal
// Hello. No control handlers, queues or connections are replayed on the new root.
func (c *Client) serveEstablished(ctx context.Context, session transport.Session, owner net.Conn, inbox *controlInbox) (*transportAttempt, error) {
	if session.Kind() != transport.TCP {
		return nil, c.serveControlInbox(owner, inbox)
	}
	controlDone := make(chan error, 1)
	go func() { controlDone <- c.serveControlInbox(owner, inbox) }()
	for {
		recoveryCtx, cancel := context.WithCancel(ctx)
		probeDone := make(chan *transportAttempt, 1)
		go func() { probeDone <- c.recoveryLoop(ctx, recoveryCtx, session, owner) }()
		select {
		case err := <-controlDone:
			cancel()
			c.discardAttempt(<-probeDone)
			return nil, err
		case candidate := <-probeDone:
			cancel()
			if candidate == nil {
				return nil, <-controlDone
			}
			if hook, _ := c.handoffHook.Load().(func()); hook != nil {
				hook()
			}
			// Recheck immediately at handoff; changes after this are rejected by
			// formal wire-Ack admission using the same epoch/credential snapshot.
			if !c.recoveryCurrent(candidate, session, owner) {
				c.discardAttempt(candidate)
				// A mode-only change advances the epoch but retains an allowed
				// owner. Keep the probe policy alive on that owner instead of
				// losing the recovery loop until the TCP session reconnects.
				if ctx.Err() == nil && c.recoveryOwnerLive(session, owner) && c.recoveryIdentityCurrent(candidate.cfg) {
					continue
				}
				return nil, <-controlDone
			}
			closeLiveConnection(owner, session)
			<-controlDone
			return candidate, nil
		}
	}
}
