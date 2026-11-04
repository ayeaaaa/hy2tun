package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/transport"
	"hy2tun/internal/version"
)

const controlHandshakeTimeout = 10 * time.Second

// credentialAckFlushGrace gives the transport a moment to flush a
// credential_ack before the control stream is closed. Closing the QUIC
// connection immediately after the write can discard the ack.
const credentialAckFlushGrace = 50 * time.Millisecond

// errClientClosed is used internally when Close wins a race with connection
// setup. Start treats it as a normal shutdown.
var errClientClosed = errors.New("client closed")

// handshake sends Hello and validates the server's HelloAck.
func (c *Client) handshake(conn net.Conn) error {
	ctx, cancel := context.WithTimeout(context.Background(), controlHandshakeTimeout)
	defer cancel()
	return c.handshakeContext(ctx, conn)
}

func (c *Client) handshakeContext(ctx context.Context, conn net.Conn) error {
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	cfg := c.snapshotConfig()
	generation := cfg.Credentials.Generation
	if generation < 1 {
		generation = 1
	}
	hello := &control.Hello{
		Type:                 control.TypeHello,
		Protocol:             control.ProtocolVersion,
		Token:                cfg.Control.Token,
		ClientID:             cfg.Client.ID,
		Name:                 cfg.Client.Name,
		Version:              version.Version,
		Revision:             cfg.Client.Revision,
		CredentialGeneration: generation,
		Capabilities:         []string{control.CapabilityCredentialUpdate},
		Mappings:             cfg.Mappings,
	}
	c.writeMu.Lock()
	err := control.WriteMessage(conn, hello)
	c.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("send control hello: %w", classifyControlError(err))
	}

	msg, err := control.ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("read control hello ack: %w", classifyControlError(err))
	}
	ack, ok := msg.(*control.HelloAck)
	if !ok {
		return fmt.Errorf("%w: unexpected control response %T", transport.ErrProtocol, msg)
	}
	if !ack.OK {
		if ack.Error == string(control.CodeUnauthorized) {
			return transport.ErrAuthentication
		}
		return transport.ErrProtocol
	}

	// Mark the handshake complete before applying the ack. Local edits that
	// arrive after this point may send updates, while applyHelloAck still
	// serializes the write-back under configMu.
	c.mu.Lock()
	if c.isClosed() || c.controlConn != conn {
		c.mu.Unlock()
		return net.ErrClosed
	}
	c.connected = true
	c.mu.Unlock()
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
			} else if err := c.writeConfig(next); err != nil {
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

	c.recordServerCredentials(ack)

	// If the hello write-back changed the file, or a local edit happened while
	// hello was in flight, announce the current config. A local edit that
	// arrives after this point sends its own update because the handshake is
	// already marked connected.
	if wrote || revisionChanged {
		c.sendUpdate()
	}
	return nil
}

// recordServerCredentials stores the credential generation and sync flag from
// HelloAck. A server that predates credential rotation omits the generation;
// its zero value and false flag mean "unknown", not generation 1.
func (c *Client) recordServerCredentials(ack *control.HelloAck) {
	if ack == nil || ack.CredentialGeneration <= 0 {
		c.mu.Lock()
		c.serverCredentialGeneration = 0
		c.credentialSynced = false
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	c.serverCredentialGeneration = ack.CredentialGeneration
	c.credentialSynced = ack.CredentialSynced
	c.mu.Unlock()
}

// serveControl processes control messages until the stream fails. Reverse UDP
// sessions are started separately after the handshake and closed when this
// stream ends.
func (c *Client) serveControl(conn net.Conn) error {
	for {
		msg, err := control.ReadMessage(conn)
		if err != nil {
			return classifyControlError(err)
		}

		switch message := msg.(type) {
		case *control.Open:
			c.log.Debug("received control open", "mapping", message.MappingID)
			go c.handleOpen(conn, message)
		case *control.Push:
			c.handlePush(conn, message)
		case *control.UpdateAck:
			c.handleUpdateAck(message)
		case *control.CredentialUpdate:
			c.handleCredentialUpdate(conn, message)
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
			return transport.ErrProtocol
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
			ID:       msg.ID,
			OK:       false,
			Error:    string(control.CodeConflict),
			Revision: current,
		})
		return
	}
	if msg.Revision <= msg.BaseRevision {
		c.configMu.Unlock()
		c.writePushAck(conn, &control.PushAck{
			ID:       msg.ID,
			OK:       false,
			Error:    string(control.CodeBadMessage),
			Revision: current,
		})
		return
	}
	if err := validatePushedMappings(msg.Mappings); err != nil {
		c.configMu.Unlock()
		c.writePushAck(conn, &control.PushAck{
			ID:       msg.ID,
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
			ID:       msg.ID,
			OK:       false,
			Error:    string(control.CodeBadMessage),
			Revision: current,
		})
		return
	}
	if err := c.writeConfig(next); err != nil {
		c.configMu.Unlock()
		c.log.Warn("apply server mapping push", "error", err)
		c.writePushAck(conn, &control.PushAck{
			ID:       msg.ID,
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
		ID:       msg.ID,
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
		} else if err := c.writeConfig(next); err != nil {
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

// handleCredentialUpdate applies a server-pushed credential pair. The config is
// written before the ack, so a successful ack always means the new values are
// durable. The control stream is then closed so the reconnect uses the new
// credentials.
func (c *Client) handleCredentialUpdate(conn net.Conn, msg *control.CredentialUpdate) {
	if conn == nil || msg == nil {
		return
	}
	ack := &control.CredentialAck{
		Type:       control.TypeCredentialAck,
		Generation: msg.Generation,
	}

	c.configMu.Lock()
	currentGeneration := c.cfg.Credentials.Generation
	if currentGeneration < 1 {
		currentGeneration = 1
	}
	switch {
	case msg.Generation <= currentGeneration:
		c.configMu.Unlock()
		ack.Error = "stale_generation"
		c.writeCredentialAck(conn, ack)
		return
	case msg.Auth == "" || msg.ControlToken == "":
		c.configMu.Unlock()
		ack.Error = "invalid_credentials"
		c.writeCredentialAck(conn, ack)
		return
	}

	next := cloneClientConfig(c.cfg)
	next.Auth = msg.Auth
	next.Control.Token = msg.ControlToken
	next.Credentials = config.CredentialsConfig{Generation: msg.Generation}
	tokens := c.udpTokenSnapshot()
	views := c.mappingSnapshot()
	if err := next.Validate(); err != nil {
		c.configMu.Unlock()
		ack.Error = "invalid_credentials"
		c.writeCredentialAck(conn, ack)
		return
	}
	if err := c.writeConfig(next); err != nil {
		c.configMu.Unlock()
		c.log.Warn("write rotated credentials", "error", err)
		ack.Error = "write_failed"
		c.writeCredentialAck(conn, ack)
		return
	}
	c.storeConfigLocked(next, tokens, views)
	c.configMu.Unlock()

	c.mu.Lock()
	c.credentialsGeneration = msg.Generation
	c.credentialSynced = false
	c.mu.Unlock()

	ack.OK = true
	if err := c.writeCredentialAck(conn, ack); err != nil {
		return
	}
	c.log.Info("credentials updated", "generation", msg.Generation)
	c.forceReconnectAfterCredentialAck(conn)
}

// forceReconnect closes the live control stream and session so the Start
// loop reconnects with the current configuration. Manual credential rotation
// has no ack to flush, so the close is immediate.
func (c *Client) forceReconnect() {
	c.mu.Lock()
	conn := c.controlConn
	session := c.session
	c.mu.Unlock()
	closeLiveConnection(conn, session)
}

// closeLiveConnection closes a captured connection pair. Either value may be
// nil when the client is offline.
func closeLiveConnection(conn net.Conn, session transport.Session) {
	if session != nil {
		_ = session.Close()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

// forceReconnectAfterCredentialAck closes the control stream so runOnce
// reconnects with the new credentials. The short grace period lets the
// transport flush the ack; closing the QUIC connection immediately after the
// write can discard the ack before the server reads it.
func (c *Client) forceReconnectAfterCredentialAck(conn net.Conn) {
	if conn == nil {
		return
	}
	// Capture the session that owns this control stream. If a newer
	// connection has already replaced it, leave that connection alone.
	c.mu.Lock()
	var session transport.Session
	if c.controlConn == conn {
		session = c.session
	}
	c.mu.Unlock()

	go func() {
		timer := time.NewTimer(credentialAckFlushGrace)
		defer timer.Stop()
		select {
		case <-timer.C:
			closeLiveConnection(conn, session)
		case <-c.closed:
		}
	}()
}

func (c *Client) writeCredentialAck(conn net.Conn, ack *control.CredentialAck) error {
	if err := c.writeControl(conn, ack); err != nil {
		c.log.Warn("send credential ack", "error", err)
		return err
	}
	return nil
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
	return classifyControlError(control.WriteMessage(conn, msg))
}

func classifyControlError(err error) error {
	if errors.Is(err, control.ErrBadMessage) || errors.Is(err, control.ErrUnknownType) || errors.Is(err, control.ErrFrameTooLarge) {
		return &transport.Error{Code: transport.ErrProtocol.Code, Cause: err}
	}
	return err
}
