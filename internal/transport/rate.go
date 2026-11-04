package transport

import (
	"context"
	"os"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const rateChunk = 16 * 1024

type byteLimiter struct{ limiter *rate.Limiter }

func newByteLimiter(bytesPerSecond uint64) *byteLimiter {
	l := &byteLimiter{}
	if bytesPerSecond != 0 {
		l.limiter = rate.NewLimiter(rate.Limit(bytesPerSecond), rateChunk)
	}
	return l
}

func (l *byteLimiter) wait(ctx context.Context, n int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if l == nil || l.limiter == nil || n == 0 {
		return nil
	}
	return l.limiter.WaitN(ctx, n)
}

// Changes to net.Conn deadlines must wake a rate wait as well as the underlying
// socket. Cancelling a WaitN refunds its reservation before retrying.
type deadlineState struct {
	mu      sync.Mutex
	at      time.Time
	changed chan struct{}
}

func (d *deadlineState) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.changed != nil {
		close(d.changed)
	}
	d.at = t
	d.changed = make(chan struct{})
}
func (d *deadlineState) snapshot() (time.Time, <-chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.changed == nil {
		d.changed = make(chan struct{})
	}
	return d.at, d.changed
}

func (l *byteLimiter) waitDeadline(ctx context.Context, n int, d *deadlineState) error {
	if l == nil || l.limiter == nil {
		return l.wait(ctx, n)
	}
	for {
		at, changed := d.snapshot()
		waitctx, cancel := context.WithCancel(ctx)
		if !at.IsZero() {
			cancel()
			waitctx, cancel = context.WithDeadline(ctx, at)
		}
		done := make(chan struct{})
		go func() {
			select {
			case <-changed:
				cancel()
			case <-done:
			}
		}()
		err := l.wait(waitctx, n)
		close(done)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-changed:
			continue
		default:
		}
		if err != nil && !at.IsZero() {
			return os.ErrDeadlineExceeded
		}
		return err
	}
}
