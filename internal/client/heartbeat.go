package client

import (
	"context"
	"net"
	"time"

	"hy2tun/internal/control"
)

type heartbeat struct {
	owner        net.Conn
	lastResponse time.Time
	lastTS       int64
	pending      bool
	wake         chan struct{}
	failed       chan error
	done         chan struct{}
}

// Called at wire acceptance under mu, before potentially slow local write-back.
func (c *Client) newHeartbeatLocked(owner net.Conn) *heartbeat {
	h := &heartbeat{owner: owner, lastResponse: c.runtime.Clock.Now(), wake: make(chan struct{}, 1), failed: make(chan error, 1), done: make(chan struct{})}
	c.heartbeat = h
	return h
}

func (c *Client) handlePong(owner net.Conn, ts int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.heartbeat
	if h == nil || c.controlConn != owner || h.owner != owner || !c.connected || !h.pending || ts != h.lastTS {
		return
	}
	h.pending = false
	h.lastResponse = c.runtime.Clock.Now()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (c *Client) runHeartbeat(ctx context.Context, h *heartbeat, closeRoot func()) {
	defer close(h.done)
	defer func() {
		c.mu.Lock()
		if c.heartbeat == h {
			c.heartbeat = nil
		}
		c.mu.Unlock()
	}()
	c.mu.Lock()
	nextPing := h.lastResponse.Add(c.runtime.Policy.HeartbeatInterval)
	c.mu.Unlock()
	var writing <-chan error
	defer func() {
		if writing != nil {
			closeRoot()
			<-writing
		}
	}()
	fail := func(err error) { h.failed <- err; closeRoot() }
	for {
		c.mu.Lock()
		if c.heartbeat != h || c.controlConn != h.owner || !c.connected {
			c.mu.Unlock()
			return
		}
		now := c.runtime.Clock.Now()
		expires := h.lastResponse.Add(c.runtime.Policy.HeartbeatTimeout)
		if !now.Before(expires) {
			c.mu.Unlock()
			fail(context.DeadlineExceeded)
			return
		}
		if !now.Before(nextPing) {
			c.mu.Unlock()
			if writing == nil {
				result := make(chan error, 1)
				writing = result
				go func() { result <- c.writeHeartbeat(h) }()
			}
			nextPing = now.Add(c.runtime.Policy.HeartbeatInterval)
			continue
		}
		wakeAt := nextPing
		if expires.Before(wakeAt) {
			wakeAt = expires
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-h.wake:
		case err := <-writing:
			writing = nil
			if err != nil {
				fail(err)
				return
			}
		case <-c.runtime.Clock.After(wakeAt.Sub(now)):
		}
	}
}

// Only one heartbeat writer is outstanding. The timer remains responsive while
// that writer waits for serialization or network I/O, and joins it on shutdown.
// Publish the association only after obtaining the writer, not while queued.
func (c *Client) writeHeartbeat(h *heartbeat) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	if c.heartbeat != h || c.controlConn != h.owner || !c.connected {
		c.mu.Unlock()
		return net.ErrClosed
	}
	c.heartbeatTS++
	h.lastTS, h.pending = c.heartbeatTS, true
	ts := h.lastTS
	c.mu.Unlock()
	return c.writeControlLocked(h.owner, &control.Ping{Type: control.TypePing, TS: ts})
}
