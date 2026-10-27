package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"

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
	return true, c.serveControl(conn)
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
