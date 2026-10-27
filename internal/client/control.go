package client

import (
	"context"
	"fmt"
	"net"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"

	"hy2tun/internal/control"
	"hy2tun/internal/version"
)

const controlHandshakeTimeout = 10 * time.Second

// runOnce establishes one HY2 connection and control stream. It returns
// whether the control handshake succeeded so Start can reset its backoff.
func (c *Client) runOnce(ctx context.Context) (bool, error) {
	hyConfig := &hyclient.Config{
		ServerAddr:  c.serverAddr,
		Auth:        c.cfg.Auth,
		TLSConfig:   c.tlsConfig,
		ConnFactory: c.connFactory,
		BandwidthConfig: hyclient.BandwidthConfig{
			MaxTx: uint64(c.cfg.Bandwidth.Up),
			MaxRx: uint64(c.cfg.Bandwidth.Down),
		},
	}

	// hyclient.NewClient does not accept a context. Run it separately so a
	// cancelled context or Close can stop the client promptly.
	type connectResult struct {
		client hyclient.Client
		err    error
	}
	connectCh := make(chan connectResult, 1)
	go func() {
		hy, _, err := hyclient.NewClient(hyConfig)
		if err == nil && (ctx.Err() != nil || c.isClosed()) {
			_ = hy.Close()
			if ctx.Err() != nil {
				err = ctx.Err()
			} else {
				err = fmt.Errorf("client closed")
			}
		}
		connectCh <- connectResult{client: hy, err: err}
	}()

	var hy hyclient.Client
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-c.closed:
		return false, nil
	case result := <-connectCh:
		if result.err != nil {
			return false, fmt.Errorf("connect HY2: %w", result.err)
		}
		hy = result.client
	}
	if ctx.Err() != nil {
		_ = hy.Close()
		return false, ctx.Err()
	}
	if c.isClosed() {
		_ = hy.Close()
		return false, nil
	}

	conn, err := hy.TCP(controlAddress)
	if err != nil {
		_ = hy.Close()
		return false, fmt.Errorf("open control stream: %w", err)
	}
	if !c.setCurrent(hy, conn) {
		_ = conn.Close()
		_ = hy.Close()
		return false, nil
	}
	defer func() {
		c.clearCurrent(hy, conn)
		_ = conn.Close()
		_ = hy.Close()
	}()

	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-c.closed:
		case <-stopWatch:
			return
		}
		_ = conn.Close()
		_ = hy.Close()
	}()
	defer close(stopWatch)

	if err := c.handshake(conn); err != nil {
		return false, err
	}
	c.markReady()
	return true, c.serveControl(conn)
}

// handshake sends Hello and validates the server's HelloAck.
func (c *Client) handshake(conn net.Conn) error {
	_ = conn.SetDeadline(time.Now().Add(controlHandshakeTimeout))
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	hello := &control.Hello{
		Type:     control.TypeHello,
		Protocol: control.ProtocolVersion,
		Token:    c.cfg.Control.Token,
		ClientID: c.cfg.Client.ID,
		Name:     c.cfg.Client.Name,
		Version:  version.Version,
		Revision: c.cfg.Client.Revision,
		Mappings: c.cfg.Mappings,
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

	c.setMappingResults(c.cfg.Mappings, ack.Results)
	c.markConnected()
	c.log.Info("control handshake complete",
		"client", c.cfg.Client.ID,
		"server_version", ack.ServerVersion,
	)
	return nil
}

// serveControl processes control messages until the stream fails. Reverse TCP
// data streams are attached in a later task.
func (c *Client) serveControl(conn net.Conn) error {
	for {
		msg, err := control.ReadMessage(conn)
		if err != nil {
			return err
		}

		switch message := msg.(type) {
		case *control.Open:
			c.log.Debug("received control open", "mapping", message.MappingID)
		case *control.Push:
			c.log.Warn("ignoring server mapping push",
				"base_revision", message.BaseRevision,
				"revision", message.Revision,
			)
		case *control.Ping:
			if err := c.writeControl(conn, &control.Pong{
				Type: control.TypePong,
				TS:   message.TS,
			}); err != nil {
				return err
			}
		case *control.PushAck, *control.Pong, *control.OpenResult, *control.UpdateAck:
			// No action is required for these messages in this task.
		default:
			c.log.Debug("ignoring unexpected control message")
		}
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
