package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	hyclient "github.com/apernet/hysteria/core/v2/client"

	"hy2tun/internal/config"
	"hy2tun/internal/transport"
)

// dialSession rebuilds endpoints/TLS/factories from this attempt's snapshot.
// In particular TCP never resolves or constructs a UDP connection factory.
func (c *Client) dialSession(ctx context.Context, kind transport.Kind, cfg *config.ClientConfig) (transport.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// This also covers address preparation, not just the final transport dial.
	dialCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-c.closed:
			cancel()
		case <-stop:
		}
	}()
	value, err := c.setupOperation(dialCtx, kind, "dial", func(dialCtx context.Context) (any, <-chan struct{}, error) {
		opts := transport.DialOptions{
			Auth: cfg.Auth, ControlToken: cfg.Control.Token, ClientID: cfg.Client.ID,
			Up: uint64(cfg.Bandwidth.Up), Down: uint64(cfg.Bandwidth.Down),
		}
		if c.runtime.Dial == nil && c.runtime.DialWithCompletion == nil {
			switch kind {
			case transport.TCP:
				addr, err := cfg.TCPServerAddress()
				if err != nil {
					return nil, nil, err
				}
				tls, err := transport.ClientTLS(cfg.TLS, addr)
				if err != nil {
					return nil, nil, err
				}
				opts.TCPAddress, opts.TLS = addr, tls
			case transport.HY2:
				addr, err := resolveServerAddrContext(dialCtx, cfg)
				if err != nil {
					return nil, nil, err
				}
				tls, err := buildTLSConfig(cfg)
				if err != nil {
					return nil, nil, err
				}
				factory, err := buildConnFactory(cfg, addr)
				if err != nil {
					return nil, nil, err
				}
				opts.HY2 = &hyclient.Config{
					ServerAddr: addr, Auth: cfg.Auth, TLSConfig: tls, ConnFactory: factory,
					BandwidthConfig: hyclient.BandwidthConfig{MaxTx: opts.Up, MaxRx: opts.Down},
				}
			default:
				return nil, nil, transport.ErrProtocol
			}
		}
		if c.runtime.DialWithCompletion != nil {
			return c.runtime.DialWithCompletion(dialCtx, kind, cloneClientConfig(cfg))
		}
		if c.runtime.Dial != nil {
			s, err := c.runtime.Dial(dialCtx, kind, cloneClientConfig(cfg))
			return s, nil, err
		}
		if c.newSessionWithCompletion != nil {
			return c.newSessionWithCompletion(dialCtx, kind, opts)
		}
		if c.newSession != nil {
			s, err := c.newSession(dialCtx, kind, opts)
			return s, nil, err
		}
		if kind == transport.TCP {
			s, err := transport.DialTCP(dialCtx, opts)
			return s, nil, err
		}
		return transport.DialHY2WithCompletion(dialCtx, opts, c.newHY2)
	}, func(value any) { _ = value.(transport.Session).Close() })
	if err != nil {
		if c.isClosed() {
			return nil, errors.Join(err, errClientClosed)
		}
		return nil, err
	}
	return value.(transport.Session), nil
}

// runSession owns an already-dialed session through control/UDP teardown. The
// caller's context is the lifetime, not a temporary establishment timeout.
func (c *Client) runSession(ctx context.Context, session transport.Session) (bool, error) {
	setupCtx, cancel := c.setupTimeout(ctx)
	defer cancel()
	return c.runSessionWithSetup(ctx, setupCtx, session, c.currentCredentialGeneration())
}

// runOnce supplies the SAME setup budget used for dial, while retaining the
// separate caller lifetime after HelloAck. Successful streams are never bound
// to the expired setup timer.
func (c *Client) runSessionWithSetup(ctx, setupCtx context.Context, session transport.Session, generation int64) (bool, error) {
	if session == nil {
		return false, transport.ErrProtocol
	}
	var conn net.Conn
	var once sync.Once
	closeBoth := func() { once.Do(func() { closeLiveConnection(conn, session) }) }
	defer closeBoth()
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	var hb *heartbeat
	defer func() {
		cancelHeartbeat()
		if hb != nil {
			<-hb.done
		}
	}()
	openCtx, cancelOpen := context.WithCancel(setupCtx)
	stopOpen := make(chan struct{})
	go func() {
		select {
		case <-c.closed:
			cancelOpen()
		case <-stopOpen:
		}
	}()
	opened, err := c.openControlSetup(openCtx, session)
	close(stopOpen)
	cancelOpen()
	conn = opened
	if err != nil {
		return false, fmt.Errorf("open control stream: %w", err)
	}
	if conn == nil {
		return false, transport.ErrProtocol
	}
	if err := setupCtx.Err(); err != nil {
		return false, err
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if !c.setCurrent(session, conn) {
		return false, errClientClosed
	}
	_, _, ownerCtx := c.sessionFor(conn)
	defer func() {
		c.clearCurrent(session, conn)
		closeBoth()
		c.closeDataFor(conn)
		c.stopUDPMappingsFor(session)
	}()
	if generation < 1 {
		generation = 1
	}
	if c.currentCredentialGeneration() != generation {
		return false, nil
	}
	setupCtx = context.WithValue(setupCtx, generationKey{}, generation)
	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-c.closed:
		case <-ownerCtx.Done():
		case <-stopWatch:
			return
		}
		closeBoth()
	}()
	defer close(stopWatch)
	// Wire acceptance and timer expiry arbitrate under one lock. A scheduled
	// callback cannot retroactively close an already-admitted root, including
	// when local persistence takes longer than the remaining setup budget.
	var admissionMu sync.Mutex
	accepted := false
	stopSetup := context.AfterFunc(setupCtx, func() {
		admissionMu.Lock()
		closeRoot := !accepted
		admissionMu.Unlock()
		if closeRoot {
			closeBoth()
		}
	})
	defer stopSetup()
	cfg, ack, err := c.handshakeWire(setupCtx, conn, func() error {
		admissionMu.Lock()
		defer admissionMu.Unlock()
		if err := setupContextError(setupCtx); err != nil {
			return err
		}
		if a, ok := setupCtx.Value(attemptKey{}).(*transportAttempt); ok {
			if err := a.admit(); err != nil {
				return err
			}
		}
		accepted = true
		stopSetup()
		c.transportAcceptedLocked(session, setupCtx)
		hb = c.newHeartbeatLocked(conn)
		go c.runHeartbeat(heartbeatCtx, hb, closeBoth)
		return nil
	})
	stopSetup()
	// Retain an observed security/protocol error even when the setup timer
	// races its read. A timeout must never replace its denied classification.
	if err != nil {
		if errors.Is(err, errStaleTransport) {
			return false, nil
		}
		if accepted {
			return false, err
		}
		return false, errors.Join(err, setupCtx.Err())
	}
	inbox := c.startControlReader(ctx, conn, closeBoth)
	defer inbox.stop()
	if err := c.completeHelloAck(cfg, ack); err != nil {
		return true, err
	}
	if current, _, _ := c.sessionFor(conn); current != session {
		return false, net.ErrClosed
	}
	c.markReady()
	c.startUDPMappings()
	candidate, err := c.serveEstablished(ctx, session, conn, inbox)
	if candidate != nil {
		if run, ok := ctx.Value(sessionRunKey{}).(*sessionRun); ok {
			run.candidate = candidate
		} else {
			c.discardAttempt(candidate)
		}
	}
	select {
	case heartbeatErr := <-hb.failed:
		err = errors.Join(err, heartbeatErr)
	default:
	}
	closeBoth()
	return true, err
}

func (c *Client) currentSession() transport.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

// sessionFor captures the exact control owner before asynchronous work. A
// stale Open can neither dial through nor attach connections to a newer root.
func (c *Client) sessionFor(owner net.Conn) (transport.Session, net.Conn, context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed() || (owner != nil && owner != c.controlConn) {
		return nil, nil, context.Background()
	}
	ctx := c.sessionCtx
	if ctx == nil {
		ctx = context.Background()
	}
	return c.session, c.controlConn, ctx
}
