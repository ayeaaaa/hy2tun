package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/version"
)

const controlHandshakeTimeout = 10 * time.Second

// errClientClosed is used internally when Close wins a race with connection
// setup. Start treats it as a normal shutdown.
var errClientClosed = errors.New("client closed")

// runOnce establishes one HY2 connection and control stream. It returns
// whether the control handshake succeeded so Start can reset its backoff.
func (c *Client) runOnce(ctx context.Context) (bool, error) {
	cfg := c.snapshotConfig()
	hyConfig := &hyclient.Config{
		ServerAddr:  c.serverAddr,
		Auth:        cfg.Auth,
		TLSConfig:   c.tlsConfig,
		ConnFactory: c.connFactory,
		BandwidthConfig: hyclient.BandwidthConfig{
			MaxTx: uint64(cfg.Bandwidth.Up),
			MaxRx: uint64(cfg.Bandwidth.Down),
		},
	}

	newHY2 := c.newHY2
	if newHY2 == nil {
		newHY2 = hyclient.NewClient
	}
	hy, err := c.connect(ctx, hyConfig, newHY2)
	if err != nil {
		return false, err
	}
	if ctx.Err() != nil {
		_ = hy.Close()
		return false, ctx.Err()
	}
	if c.isClosed() {
		_ = hy.Close()
		return false, nil
	}

	conn, err := c.openControlStream(ctx, hy)
	if err != nil {
		_ = hy.Close()
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if c.isClosed() {
			return false, nil
		}
		return false, fmt.Errorf("open control stream: %w", err)
	}
	if ctx.Err() != nil {
		_ = conn.Close()
		_ = hy.Close()
		return false, ctx.Err()
	}
	if c.isClosed() {
		_ = conn.Close()
		_ = hy.Close()
		return false, nil
	}

	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = conn.Close()
			_ = hy.Close()
		})
	}

	if !c.setCurrent(hy, conn) {
		closeBoth()
		return false, nil
	}
	defer func() {
		c.clearCurrent(hy, conn)
		c.closeDataFor(conn)
		closeBoth()
	}()

	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-c.closed:
		case <-stopWatch:
			return
		}
		closeBoth()
	}()
	defer close(stopWatch)

	if err := c.handshake(conn); err != nil {
		return false, err
	}
	c.markReady()
	c.startUDPMappings()
	defer c.stopUDPMappings()

	serveErr := c.serveControl(conn)
	// Close the HY2 connection before waiting for UDP goroutines: a datagram
	// send may be blocked on a full QUIC send queue and only unblocks when the
	// connection closes.
	closeBoth()
	return true, serveErr
}

// connect runs hyclient.NewClient in a goroutine so cancellation and Close can
// stop it. The result channel is unbuffered and the goroutine closes any client
// it cannot hand off, so a racing shutdown cannot leak a HY2 client.
func (c *Client) connect(ctx context.Context, cfg *hyclient.Config, newHY2 func(*hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error)) (hyclient.Client, error) {
	type connectResult struct {
		client hyclient.Client
		err    error
	}
	resultCh := make(chan connectResult)

	go func() {
		hy, _, err := newHY2(cfg)
		if err == nil && (ctx.Err() != nil || c.isClosed()) {
			_ = hy.Close()
			if ctx.Err() != nil {
				err = ctx.Err()
			} else {
				err = errClientClosed
			}
		}
		if err != nil {
			select {
			case resultCh <- connectResult{err: err}:
			case <-ctx.Done():
			case <-c.closed:
			}
			return
		}
		select {
		case resultCh <- connectResult{client: hy}:
		case <-ctx.Done():
			_ = hy.Close()
		case <-c.closed:
			_ = hy.Close()
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, errClientClosed
	case result := <-resultCh:
		if result.err != nil {
			return nil, fmt.Errorf("connect HY2: %w", result.err)
		}
		return result.client, nil
	}
}

// openControlStream opens the control stream in a goroutine. Closing the HY2
// client on cancellation unblocks a stalled TCP stream request.
func (c *Client) openControlStream(ctx context.Context, hy hyclient.Client) (net.Conn, error) {
	type streamResult struct {
		conn net.Conn
		err  error
	}
	resultCh := make(chan streamResult)

	go func() {
		conn, err := hy.TCP(controlAddress)
		if err == nil && conn == nil {
			err = fmt.Errorf("control stream is nil")
		}
		if err == nil && (ctx.Err() != nil || c.isClosed()) {
			_ = conn.Close()
			conn = nil
			if ctx.Err() != nil {
				err = ctx.Err()
			} else {
				err = errClientClosed
			}
		}
		if err != nil {
			select {
			case resultCh <- streamResult{err: err}:
			case <-ctx.Done():
			case <-c.closed:
			}
			return
		}
		select {
		case resultCh <- streamResult{conn: conn}:
		case <-ctx.Done():
			_ = conn.Close()
		case <-c.closed:
			_ = conn.Close()
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		return nil, errClientClosed
	case result := <-resultCh:
		return result.conn, result.err
	}
}

// handshake sends Hello and validates the server's HelloAck.
func (c *Client) handshake(conn net.Conn) error {
	_ = conn.SetDeadline(time.Now().Add(controlHandshakeTimeout))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	cfg := c.snapshotConfig()
	hello := &control.Hello{
		Type:     control.TypeHello,
		Protocol: control.ProtocolVersion,
		Token:    cfg.Control.Token,
		ClientID: cfg.Client.ID,
		Name:     cfg.Client.Name,
		Version:  version.Version,
		Revision: cfg.Client.Revision,
		Mappings: cfg.Mappings,
	}
	if err := c.writeControl(conn, hello); err != nil {
		return fmt.Errorf("send control hello: %w", err)
	}

	msg, err := control.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("read control hello ack: %w", err)
	}
	ack, ok := msg.(*control.HelloAck)
	if !ok {
		return fmt.Errorf("unexpected control response %T", msg)
	}
	if !ack.OK {
		return fmt.Errorf("control handshake rejected: %s", ack.Error)
	}

	// Mark the handshake complete before applying the ack. Local edits that
	// arrive after this point may send updates, while applyHelloAck still
	// serializes the write-back under configMu.
	c.markConnected()
	if err := c.applyHelloAck(cfg, ack); err != nil {
		return err
	}
	c.log.Info("control handshake complete",
		"client", cfg.Client.ID,
		"server_version", ack.ServerVersion,
	)
	return nil
}

// applyHelloAck records server-assigned ports. The first assignment bumps the
// revision and is announced immediately with an update. The write-back is done
// under configMu and only when the snapshot revision is still current, so a
// concurrent local edit can never be overwritten by the stale hello snapshot.
func (c *Client) applyHelloAck(snapshot *config.ClientConfig, ack *control.HelloAck) error {
	c.configMu.Lock()
	current := c.cfg
	revisionChanged := current.Client.Revision != snapshot.Client.Revision
	next := cloneClientConfig(current)
	wrote := false

	// Only write the hello snapshot's assigned ports when no local edit
	// advanced the revision. If it did, the current config wins; the update
	// sent below lets the server return the assigned ports in update_ack, and
	// handleUpdateAck persists them without another bump.
	if !revisionChanged {
		mappings, portsChanged := applyAssignedPorts(next.Mappings, ack.Results)
		next.Mappings = mappings
		if portsChanged {
			next.Client.Revision = current.Client.Revision + 1
			if err := next.Validate(); err != nil {
				c.log.Warn("write server-assigned ports", "error", err)
				next = cloneClientConfig(current)
			} else if err := writeConfigAtomic(c.configPath, next); err != nil {
				c.log.Warn("write server-assigned ports", "error", err)
				next = cloneClientConfig(current)
			} else {
				wrote = true
			}
		}
	}

	tokens := mergeHelloTokens(snapshot.Mappings, next.Mappings, ack.Results)
	views := helloViews(snapshot.Mappings, next.Mappings, ack.Results)
	c.storeConfigLocked(next, tokens, views)
	c.configMu.Unlock()

	// If the hello write-back changed the file, or a local edit happened while
	// hello was in flight, announce the current config. A local edit that
	// arrives after this point sends its own update because the handshake is
	// already marked connected.
	if wrote || revisionChanged {
		c.sendUpdate()
	}
	return nil
}

// serveControl processes control messages until the stream fails. Reverse UDP
// sessions are started separately after the handshake and closed when this
// stream ends.
func (c *Client) serveControl(conn net.Conn) error {
	for {
		msg, err := control.ReadMessage(conn)
		if err != nil {
			return err
		}

		switch message := msg.(type) {
		case *control.Open:
			c.log.Debug("received control open", "mapping", message.MappingID)
			go c.handleOpen(message)
		case *control.Push:
			c.handlePush(conn, message)
		case *control.UpdateAck:
			c.handleUpdateAck(message)
		case *control.Ping:
			if err := c.writeControl(conn, &control.Pong{
				Type: control.TypePong,
				TS:   message.TS,
			}); err != nil {
				return err
			}
		case *control.PushAck, *control.Pong, *control.OpenResult, *control.HelloAck:
			// No action is required for these messages.
		default:
			c.log.Debug("ignoring unexpected control message")
		}
	}
}

// handlePush applies an authoritative mapping list from the server. The client
// revision is authoritative: the push only applies when its base revision
// still matches, and the applied revision is exactly the push revision.
func (c *Client) handlePush(conn net.Conn, msg *control.Push) {
	if conn == nil || msg == nil {
		return
	}

	c.configMu.Lock()
	current := c.cfg.Client.Revision
	if msg.BaseRevision != current {
		c.configMu.Unlock()
		c.writePushAck(conn, &control.PushAck{
			OK:       false,
			Error:    string(control.CodeConflict),
			Revision: current,
		})
		return
	}
	if msg.Revision <= msg.BaseRevision {
		c.configMu.Unlock()
		c.writePushAck(conn, &control.PushAck{
			OK:       false,
			Error:    string(control.CodeBadMessage),
			Revision: current,
		})
		return
	}
	if err := validatePushedMappings(msg.Mappings); err != nil {
		c.configMu.Unlock()
		c.writePushAck(conn, &control.PushAck{
			OK:       false,
			Error:    string(control.CodeBadMessage),
			Revision: current,
		})
		return
	}

	next := cloneClientConfig(c.cfg)
	next.Mappings = append([]config.Mapping(nil), msg.Mappings...)
	next.Client.Revision = msg.Revision
	if err := next.Validate(); err != nil {
		c.configMu.Unlock()
		c.log.Warn("validate server mapping push", "error", err)
		c.writePushAck(conn, &control.PushAck{
			OK:       false,
			Error:    string(control.CodeBadMessage),
			Revision: current,
		})
		return
	}
	if err := writeConfigAtomic(c.configPath, next); err != nil {
		c.configMu.Unlock()
		c.log.Warn("apply server mapping push", "error", err)
		c.writePushAck(conn, &control.PushAck{
			OK:       false,
			Error:    "write_failed",
			Revision: current,
		})
		return
	}

	tokens := retainTokensForPush(c.cfg.Mappings, next.Mappings, c.udpTokenSnapshot(), c.mappingSnapshot())
	applyPushTokens(tokens, next.Mappings, msg.Results)
	views := pushViews(next.Mappings, tokens)
	c.storeConfigLocked(next, tokens, views)
	c.configMu.Unlock()

	c.writePushAck(conn, &control.PushAck{
		OK:       true,
		Revision: msg.Revision,
		Results:  pushResults(next.Mappings, tokens),
	})
	c.reconcileUDPMappings()
}

func (c *Client) writePushAck(conn net.Conn, ack *control.PushAck) {
	ack.Type = control.TypePushAck
	if err := c.writeControl(conn, ack); err != nil {
		c.log.Warn("send push ack", "error", err)
	}
}

// handleUpdateAck writes assigned ports back to the configuration without
// bumping the revision: the assignment belongs to the update that is already
// in flight.
func (c *Client) handleUpdateAck(ack *control.UpdateAck) {
	if ack == nil || len(ack.Results) == 0 {
		return
	}
	if len(ack.Results) == 1 && ack.Results[0].ID == "" {
		c.log.Debug("control update rejected",
			"revision", ack.Revision,
			"error", ack.Results[0].Error,
		)
		return
	}

	c.configMu.Lock()
	current := c.cfg.Client.Revision
	c.configMu.Unlock()
	switch {
	case ack.Revision < current:
		// The server rolled the whole update back. Keep running mappings and
		// their UDP tokens; only mappings that are not already running can be
		// marked failed.
		c.log.Debug("control update rolled back",
			"ack_revision", ack.Revision,
			"revision", current,
		)
		c.applyRejectedUpdateResults(ack.Results)
		return
	case ack.Revision > current:
		c.log.Debug("ignoring update ack for a different revision",
			"ack_revision", ack.Revision,
			"revision", current,
		)
		return
	}

	c.configMu.Lock()
	next := cloneClientConfig(c.cfg)
	mappings, changed := applyAssignedPorts(next.Mappings, ack.Results)
	effective := next.Mappings
	applied := false
	if changed {
		next.Mappings = mappings
		if err := next.Validate(); err != nil {
			c.log.Warn("write update-assigned ports", "error", err)
		} else if err := writeConfigAtomic(c.configPath, next); err != nil {
			c.log.Warn("write update-assigned ports", "error", err)
		} else {
			views := mappingViews(next.Mappings, resultMap(ack.Results))
			c.storeConfigLocked(next, tokensFromResults(ack.Results), views)
			effective = next.Mappings
			applied = true
		}
	}
	c.configMu.Unlock()

	if !applied {
		c.setMappingResults(effective, ack.Results)
	}
	c.reconcileUDPMappings()
}

// applyRejectedUpdateResults marks mappings that are not already running as
// failed after the server rolled an update back. Running mappings keep their
// status and UDP token because the server left the previous mapping set in
// place.
func (c *Client) applyRejectedUpdateResults(results []control.Result) {
	byID := resultMap(results)

	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.mappings {
		result, ok := byID[c.mappings[i].ID]
		if !ok || result.Error == "" || c.mappings[i].Status == "running" {
			continue
		}
		c.mappings[i].Status = "failed"
		c.mappings[i].Error = result.Error
	}
}

// writeControl serializes and bounds one frame written to the control stream.
func (c *Client) writeControl(conn net.Conn, msg any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	_ = conn.SetWriteDeadline(time.Now().Add(controlHandshakeTimeout))
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	return control.WriteMessage(conn, msg)
}
