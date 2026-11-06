package client

import (
	"context"
	"net"

	"hy2tun/internal/control"
	"hy2tun/internal/transport"
)

// Only this reader accesses an established control stream. Heartbeat responses
// bypass slow local write-back; business messages stay in ordered bounded
// admission until Ack application/each earlier handler completes. Overflow is
// explicit local backpressure and closes the owner, never a silent drop/replay.
const maxQueuedControlMessages = 64

type controlInbox struct {
	messages chan any
	terminal chan error
	done     chan struct{}
	stop     func()
}

func (c *Client) controlOwnerCurrent(owner net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.isClosed() && c.connected && c.controlConn == owner
}

func (c *Client) startControlReader(parent context.Context, owner net.Conn, closeRoot func()) *controlInbox {
	ctx, cancel := context.WithCancel(parent)
	inbox := &controlInbox{messages: make(chan any, maxQueuedControlMessages), terminal: make(chan error, 1), done: make(chan struct{})}
	inbox.stop = func() { cancel(); closeRoot(); <-inbox.done }
	go func() {
		defer close(inbox.done)
		fail := func(err error) { inbox.terminal <- err; closeRoot() }
		for {
			msg, err := control.ReadMessage(owner)
			if err != nil {
				fail(classifyControlError(err))
				return
			}
			if !c.controlOwnerCurrent(owner) {
				fail(net.ErrClosed)
				return
			}
			switch message := msg.(type) {
			case *control.Pong:
				c.handlePong(owner, message.TS)
				continue
			case *control.Ping:
				if err := c.writeControl(owner, &control.Pong{Type: control.TypePong, TS: message.TS}); err != nil {
					fail(err)
					return
				}
				continue
			case *control.PushAck, *control.OpenResult, *control.HelloAck:
				continue
			case *control.Open, *control.Push, *control.UpdateAck, *control.CredentialUpdate, *control.ExposureUpdate:
			default:
				fail(transport.ErrProtocol)
				return
			}
			select {
			case <-ctx.Done():
				fail(ctx.Err())
				return
			case inbox.messages <- msg:
			default:
				fail(transport.ErrQueueFull)
				return
			}
		}
	}()
	return inbox
}

func (c *Client) serveControlInbox(owner net.Conn, inbox *controlInbox) error {
	for {
		// A completed old reader's queued work cannot replay after owner loss.
		select {
		case err := <-inbox.terminal:
			return err
		default:
		}
		select {
		case err := <-inbox.terminal:
			return err
		case msg := <-inbox.messages:
			select {
			case <-inbox.done:
				return <-inbox.terminal
			default:
			}
			if !c.controlOwnerCurrent(owner) {
				return net.ErrClosed
			}
			if err := c.dispatchControl(owner, msg); err != nil {
				return err
			}
		}
	}
}
