package client

import (
	"context"
	"errors"
	"fmt"
	"net"

	"hy2tun/internal/transport"
)

type setupOperation struct {
	kind      transport.Kind
	operation string
}

var errSetupBusy = fmt.Errorf("pending native setup work: %w", transport.ErrQueueFull)

// Only our local reservation backpressure may reuse an earlier same-epoch
// network fallback decision. A joined auth/TLS/protocol/cancellation error is
// not pure backpressure and can never acquire that permission.
func setupBusyOnly(err error) bool {
	if err == errSetupBusy {
		return true
	}
	switch err := err.(type) {
	case interface{ Unwrap() []error }:
		causes := err.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !setupBusyOnly(cause) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return setupBusyOnly(err.Unwrap())
	default:
		return false
	}
}

// One reservation per kind/operation, retained across root replacement until
// native completion AND outer abandoned-result cleanup. Separate TCP admission
// means stuck HY2 setup work cannot consume TCP's dial capacity.
func (c *Client) setupOperation(ctx context.Context, kind transport.Kind, operation string,
	open func(context.Context) (any, <-chan struct{}, error), cleanup func(any)) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.isClosed() {
		c.mu.Unlock()
		return nil, errClientClosed
	}
	if c.setupSlots == nil {
		c.setupSlots = make(map[setupOperation]chan struct{})
	}
	key := setupOperation{kind, operation}
	slots := c.setupSlots[key]
	if slots == nil {
		slots = make(chan struct{}, 1)
		c.setupSlots[key] = slots
	}
	c.mu.Unlock()
	select {
	case slots <- struct{}{}:
	default:
		return nil, errSetupBusy
	}
	type result struct {
		value any
		err   error
	}
	results := make(chan result)
	admitted := make(chan bool, 1)
	finished := make(chan struct{})
	go func() {
		var completion <-chan struct{}
		defer func() {
			if completion != nil {
				<-completion
			}
			<-slots
			close(finished)
		}()
		value, done, err := open(ctx)
		completion = done
		err = errors.Join(err, ctx.Err())
		if err == nil && value == nil {
			err = transport.ErrProtocol
		}
		// Success is delivered only after native handoff completion. Waiting
		// stays in this reserved worker; cancelled callers still return promptly.
		if err == nil && completion != nil {
			select {
			case <-completion:
			case <-ctx.Done():
				err = ctx.Err()
			}
		}
		if err != nil && value != nil {
			cleanup(value)
			value = nil
		}
		select {
		case results <- result{value, err}:
			if !<-admitted && value != nil {
				cleanup(value)
			}
		case <-ctx.Done():
			if value != nil {
				cleanup(value)
			}
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-results:
		err := errors.Join(res.err, ctx.Err())
		admitted <- err == nil
		if err != nil {
			return nil, err
		}
		// Positive admission has no remaining I/O/cleanup; release capacity
		// before allowing the next sequential setup step to race its bookkeeping.
		<-finished
		return res.value, nil
	}
}

func (c *Client) openControlSetup(ctx context.Context, session transport.Session) (net.Conn, error) {
	value, err := c.setupOperation(ctx, session.Kind(), "control", func(ctx context.Context) (any, <-chan struct{}, error) {
		var conn net.Conn
		var done <-chan struct{}
		var err error
		if tracked, ok := session.(transport.ControlCompletionSession); ok {
			conn, done, err = tracked.OpenControlWithCompletion(ctx)
		} else {
			conn, err = session.OpenControl(ctx)
		}
		if conn == nil {
			return nil, done, err
		}
		return conn, done, err
	}, func(value any) { _ = value.(net.Conn).Close() })
	if err != nil {
		return nil, err
	}
	return value.(net.Conn), nil
}

func (c *Client) probeSetup(ctx context.Context, session transport.Session) error {
	_, err := c.setupOperation(ctx, session.Kind(), "probe", func(ctx context.Context) (any, <-chan struct{}, error) {
		if tracked, ok := session.(transport.ProbeCompletionSession); ok {
			return tracked.ProbeWithCompletion(ctx)
		}
		info, err := session.Probe(ctx)
		return info, nil, err
	}, func(any) {})
	return err
}
