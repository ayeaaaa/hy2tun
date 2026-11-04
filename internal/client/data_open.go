package client

import (
	"context"
	"errors"
	"io"
	"net"

	"hy2tun/internal/transport"
)

const maxPendingHY2DataOpens = 1024

func (c *Client) openTCPData(ctx, ownerCtx context.Context, session transport.Session, req transport.TCPRequest) (net.Conn, error) {
	child, err := c.openData(ctx, ownerCtx, session, func(ctx context.Context) (io.Closer, error) {
		return session.OpenTCP(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return child.(net.Conn), nil
}

func (c *Client) openUDPData(ctx, ownerCtx context.Context, session transport.Session, token string) (transport.DatagramConn, error) {
	child, err := c.openData(ctx, ownerCtx, session, func(ctx context.Context) (io.Closer, error) {
		return session.OpenUDP(ctx, token)
	})
	if err != nil {
		return nil, err
	}
	return child.(transport.DatagramConn), nil
}

// Established HY2 data operations cannot pass request deadlines to the adapter:
// cancelling that context owns the entire HY2 root. Instead only owner teardown
// cancels the native open, and a bounded worker owns its late result/cleanup.
// TCP supports per-child cancellation natively and keeps its existing budget.
func (c *Client) openData(ctx, ownerCtx context.Context, session transport.Session, open func(context.Context) (io.Closer, error)) (io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ownerCtx.Err(); err != nil {
		return nil, err
	}
	if session.Kind() != transport.HY2 {
		openCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(ownerCtx, cancel)
		defer stop()
		defer cancel()
		child, err := open(openCtx)
		return finishDataOpen(child, errors.Join(err, openCtx.Err()))
	}
	c.mu.Lock()
	if c.isClosed() {
		c.mu.Unlock()
		return nil, net.ErrClosed
	}
	if c.hy2DataOpenSlots == nil {
		c.hy2DataOpenSlots = make(chan struct{}, maxPendingHY2DataOpens)
	}
	slots := c.hy2DataOpenSlots
	c.mu.Unlock()
	select {
	case slots <- struct{}{}:
	default:
		return nil, transport.ErrQueueFull
	}
	type result struct {
		child io.Closer
		err   error
	}
	results := make(chan result)
	// Caller admission decides whether the child was actually delivered. The
	// worker retains cleanup/slot ownership across a cancellation racing handoff.
	admitted := make(chan bool, 1)
	go func() {
		// Do not release on caller cancellation: an uncompleted native open or
		// blocking late-child Close still consumes its original worker budget.
		defer func() { <-slots }()
		child, err := open(ownerCtx)
		child, err = finishDataOpen(child, errors.Join(err, ctx.Err(), ownerCtx.Err()))
		select {
		case results <- result{child, err}:
			if !<-admitted && child != nil {
				_ = child.Close()
			}
		case <-ctx.Done():
			if child != nil {
				_ = child.Close()
			}
		case <-ownerCtx.Done():
			if child != nil {
				_ = child.Close()
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ownerCtx.Done():
		return nil, ownerCtx.Err()
	case res := <-results:
		err := errors.Join(res.err, ctx.Err(), ownerCtx.Err())
		admitted <- err == nil
		if err != nil {
			return nil, err
		}
		return res.child, nil
	}
}

func finishDataOpen(child io.Closer, err error) (io.Closer, error) {
	if err != nil {
		if child != nil {
			_ = child.Close()
		}
		return nil, err
	}
	if child == nil {
		return nil, transport.ErrProtocol
	}
	return child, nil
}
