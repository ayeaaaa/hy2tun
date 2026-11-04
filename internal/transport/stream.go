package transport

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatedier/yamux"
)

const (
	StreamControl    uint8 = 1
	StreamReverseTCP uint8 = 2
	StreamUDP        uint8 = 3
	StreamTCPProxy   uint8 = 4
)

type StreamHeader struct {
	Kind   uint8  `json:"kind"`
	Token  string `json:"token"`
	Target string `json:"target"`
}

type streamAck struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func validStreamHeader(h StreamHeader) bool {
	if len(h.Token)+len(h.Target) > streamFrameLimit {
		return false
	}
	switch h.Kind {
	case StreamControl:
		return h.Token == "" && h.Target == ""
	case StreamReverseTCP, StreamUDP:
		return validHY2Token(h.Token) && h.Target == ""
	case StreamTCPProxy:
		_, port, err := net.SplitHostPort(h.Target)
		return h.Token == "" && err == nil && port != ""
	default:
		return false
	}
}

// WriteStreamAck must precede business I/O on an accepted stream. It bypasses
// data rate limiting, and exposes only a fixed safe error code to the peer.
func WriteStreamAck(conn net.Conn, err error) error {
	if conn == nil {
		return ErrProtocol
	}
	raw := conn
	if c, ok := conn.(*managedConn); ok {
		raw = c.Conn
	}
	if e := raw.SetWriteDeadline(time.Now().Add(handshakeTimeout)); e != nil {
		return e
	}
	e := writeJSONFrame(raw, streamAck{OK: err == nil, Error: wireError(err)}, streamFrameLimit)
	_ = raw.SetWriteDeadline(time.Time{})
	return e
}

func (s *tcpSession) OpenControl(ctx context.Context) (net.Conn, error) {
	return s.openStream(ctx, StreamHeader{Kind: StreamControl})
}

func (s *tcpSession) OpenTCP(ctx context.Context, req TCPRequest) (net.Conn, error) {
	if (req.ReverseToken == "") == (req.Target == "") {
		return nil, ErrProtocol
	}
	h := StreamHeader{Kind: StreamTCPProxy, Target: req.Target}
	if req.ReverseToken != "" {
		h = StreamHeader{Kind: StreamReverseTCP, Token: req.ReverseToken}
	}
	return s.openStream(ctx, h)
}

func (s *tcpSession) OpenUDP(ctx context.Context, token string) (DatagramConn, error) {
	c, err := s.openStream(ctx, StreamHeader{Kind: StreamUDP, Token: token})
	if err != nil {
		return nil, err
	}
	return NewDatagramConn(c), nil
}

// A reservation covers both a pending worker and any connection/cleanup it
// creates. The slot is returned only after both lifetimes have ended.
type streamReservation struct {
	session *tcpSession
	refs    atomic.Int32
}

func (r *streamReservation) retain() { r.refs.Add(1) }
func (r *streamReservation) release() {
	if r.refs.Add(-1) == 0 {
		<-r.session.slots
	}
}

func (s *tcpSession) takeSlot(ctx context.Context, create bool) (*streamReservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.ctx.Err() != nil || s.mux.IsClosed() {
		return nil, s.connectionError(net.ErrClosed)
	}
	// Outbound creation adds a mux stream. Inbound acceptance only claims a
	// stream already counted by yamux (including its bounded accept backlog).
	if create && s.mux.NumStreams() >= maxTCPStreams {
		return nil, ErrQueueFull
	}
	select {
	case s.slots <- struct{}{}:
		r := &streamReservation{session: s}
		r.refs.Store(1)
		return r, nil
	default:
		return nil, ErrQueueFull
	}
}

// OpenStream in the pinned yamux has no context. The handoff is unbuffered and
// the slot remains owned by the worker until late completion, so repeated
// cancellation cannot create unbounded blocked open goroutines.
func (s *tcpSession) openStream(ctx context.Context, h StreamHeader) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validStreamHeader(h) {
		return nil, ErrProtocol
	}
	opctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	slot, err := s.takeSlot(opctx, true)
	if err != nil {
		return nil, err
	}
	type result struct {
		c   *managedConn
		err error
	}
	results := make(chan result)
	go func() {
		defer slot.release()
		raw, err := s.mux.OpenStream()
		if err == nil {
			c := s.manage(raw, h.Kind, slot)
			stop := context.AfterFunc(opctx, c.cancel)
			deadline, _ := opctx.Deadline()
			_ = raw.SetDeadline(deadline)
			err = writeJSONFrame(raw, h, streamFrameLimit)
			var ack streamAck
			if err == nil {
				err = readJSONFrame(raw, &ack, streamFrameLimit)
			}
			if err == nil {
				if ack.OK {
					if ack.Error != "" {
						err = ErrProtocol
					}
				} else if ack.Error == "unauthorized" {
					err = ErrAuthentication
				} else {
					err = ErrProtocol
				}
			}
			stopped := stop()
			if err == nil && (!stopped || opctx.Err() != nil) {
				err = contextError(opctx, net.ErrClosed)
			}
			if err == nil && s.ctx.Err() != nil {
				err = s.connectionError(net.ErrClosed)
			}
			if err == nil {
				_ = raw.SetDeadline(time.Time{})
			}
			if err != nil {
				c.cancel()
			} else {
				select {
				case results <- result{c: c}:
					return
				case <-opctx.Done():
				case <-s.ctx.Done():
				}
				c.cancel()
				return
			}
		}
		select {
		case results <- result{err: err}:
		case <-opctx.Done():
		case <-s.ctx.Done():
		}
	}()
	select {
	case <-opctx.Done():
		return nil, opctx.Err()
	case <-s.ctx.Done():
		return nil, s.connectionError(net.ErrClosed)
	case r := <-results:
		if err := opctx.Err(); err != nil {
			if r.c != nil {
				r.c.cancel()
			}
			return nil, err
		}
		if s.ctx.Err() != nil {
			if r.c != nil {
				r.c.cancel()
			}
			if r.err != nil {
				return nil, s.connectionError(r.err)
			}
			return nil, s.connectionError(net.ErrClosed)
		}
		if r.err != nil {
			if r.c != nil {
				r.c.cancel()
			}
			return nil, s.connectionError(r.err)
		}
		if r.c == nil {
			return nil, ErrProtocol
		}
		return r.c, nil
	}
}

func (p *TCPPeer) Accept(ctx context.Context) (StreamHeader, net.Conn, error) {
	s := p.session
	slot, err := s.takeSlot(ctx, false)
	if err != nil {
		return StreamHeader{}, nil, err
	}
	type result struct {
		h   StreamHeader
		c   *managedConn
		err error
	}
	results := make(chan result)
	go func() {
		defer slot.release()
		h, c, err := s.acceptStream(ctx, slot)
		select {
		case results <- result{h: h, c: c, err: err}:
			return
		case <-ctx.Done():
		case <-s.ctx.Done():
		}
		if c != nil {
			c.cancel()
		}
	}()
	select {
	case <-ctx.Done():
		return StreamHeader{}, nil, ctx.Err()
	case <-s.ctx.Done():
		return StreamHeader{}, nil, s.connectionError(net.ErrClosed)
	case r := <-results:
		if err := ctx.Err(); err != nil {
			if r.c != nil {
				r.c.cancel()
			}
			return StreamHeader{}, nil, err
		}
		if s.ctx.Err() != nil {
			if r.c != nil {
				r.c.cancel()
			}
			if r.err != nil {
				return StreamHeader{}, nil, s.connectionError(r.err)
			}
			return StreamHeader{}, nil, s.connectionError(net.ErrClosed)
		}
		// Do not convert a nil *managedConn to a non-nil net.Conn interface.
		if r.err != nil {
			if r.c != nil {
				r.c.cancel()
			}
			return StreamHeader{}, nil, s.connectionError(r.err)
		}
		if r.c == nil {
			return StreamHeader{}, nil, ErrProtocol
		}
		return r.h, r.c, nil
	}
}

// The caller's cancellation wait is outside the worker because even yamux's
// context-aware accept can block writing its committed mux ACK. The reservation
// remains held until that worker and any child reset/close finish.
func (s *tcpSession) acceptStream(ctx context.Context, slot *streamReservation) (StreamHeader, *managedConn, error) {
	raw, err := s.mux.AcceptStreamWithContext(ctx)
	if err != nil {
		return StreamHeader{}, nil, contextError(ctx, s.connectionError(err))
	}
	c := s.manage(raw, StreamControl, slot)
	opctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	stop := context.AfterFunc(opctx, c.cancel)
	deadline, _ := opctx.Deadline()
	_ = raw.SetDeadline(deadline)
	var h StreamHeader
	err = readJSONFrame(raw, &h, streamFrameLimit)
	if err == nil && !validStreamHeader(h) {
		err = ErrProtocol
	}
	if err != nil {
		if opctx.Err() != nil {
			stop()
			c.cancel()
			return StreamHeader{}, nil, opctx.Err()
		}
		if errors.Is(err, ErrProtocol) {
			_ = WriteStreamAck(raw, ErrProtocol)
		}
		stop()
		_ = c.Close()
		return StreamHeader{}, nil, contextError(opctx, err)
	}
	if !stop() || opctx.Err() != nil {
		c.cancel()
		return StreamHeader{}, nil, contextError(opctx, net.ErrClosed)
	}
	_ = raw.SetReadDeadline(time.Time{})
	if h.Kind != StreamControl {
		c.readLimit = s.readLimit
		c.writeLimit = s.writeLimit
	}
	// Keep the write deadline until the upper layer sends its application ack.
	return h, c, nil
}

type managedConn struct {
	net.Conn
	session                     *tcpSession
	ctx                         context.Context
	cancel                      context.CancelFunc
	readLimit, writeLimit       *byteLimiter
	closeOnce                   sync.Once
	closeErr                    error
	release                     func()
	readDeadline, writeDeadline deadlineState
	readMu                      sync.Mutex
	readBuffer, readPending     []byte
	readPendingErr              error
}

func (s *tcpSession) manage(raw *yamux.Stream, kind uint8, slot *streamReservation) *managedConn {
	slot.retain()
	ctx, cancel := context.WithCancel(s.ctx)
	c := &managedConn{Conn: raw, session: s, ctx: ctx, cancel: cancel, release: slot.release}
	if kind != StreamControl {
		c.readLimit = s.readLimit
		c.writeLimit = s.writeLimit
	}
	context.AfterFunc(ctx, func() { _ = c.abort() })
	return c
}

func (c *managedConn) shutdown(reset bool) error {
	c.closeOnce.Do(func() {
		c.cancel()
		if reset {
			c.closeErr = c.Conn.(*yamux.Stream).Reset()
		} else {
			// Close is graceful for already-written bytes, but releases local reads
			// like net.Conn.Close rather than yamux's half-close semantics.
			_ = c.Conn.SetReadDeadline(time.Now())
			c.closeErr = c.Conn.Close()
		}
		if c.release != nil {
			c.release()
		}
	})
	return c.closeErr
}
func (c *managedConn) Close() error { return c.shutdown(false) }
func (c *managedConn) abort() error { return c.shutdown(true) }

func (c *managedConn) Read(p []byte) (int, error) {
	if c.ctx.Err() != nil {
		return 0, c.session.connectionError(net.ErrClosed)
	}
	if c.readLimit == nil || c.readLimit.limiter == nil {
		n, err := c.Conn.Read(p)
		return n, c.session.connectionError(err)
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.ctx.Err() != nil {
		return 0, c.session.connectionError(net.ErrClosed)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if len(c.readPending) == 0 {
		if c.readBuffer == nil {
			c.readBuffer = make([]byte, rateChunk)
		}
		n, err := c.Conn.Read(c.readBuffer[:min(len(p), rateChunk)])
		if n == 0 {
			return 0, c.session.connectionError(err)
		}
		c.readPending, c.readPendingErr = c.readBuffer[:n], err
	}
	// Keep unreported read-ahead private and bounded. A failed rate wait must
	// neither expose these bytes nor let a retry skip them or read newer data.
	n := min(len(p), len(c.readPending))
	if err := c.readLimit.waitDeadline(c.ctx, n, &c.readDeadline); err != nil {
		return 0, c.session.connectionError(err)
	}
	copy(p, c.readPending[:n])
	c.readPending = c.readPending[n:]
	var err error
	if len(c.readPending) == 0 {
		err = c.readPendingErr
		c.readPendingErr = nil
	}
	return n, c.session.connectionError(err)
}

func (c *managedConn) Write(p []byte) (int, error) {
	if c.ctx.Err() != nil {
		return 0, c.session.connectionError(net.ErrClosed)
	}
	if c.writeLimit == nil {
		n, err := c.Conn.Write(p)
		return n, c.session.connectionError(err)
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > rateChunk {
			n = rateChunk
		}
		if err := c.writeLimit.waitDeadline(c.ctx, n, &c.writeDeadline); err != nil {
			return total, c.session.connectionError(err)
		}
		written, err := c.Conn.Write(p[:n])
		total += written
		p = p[written:]
		if err != nil {
			return total, c.session.connectionError(err)
		}
		if written == 0 {
			return total, net.ErrClosed
		}
	}
	return total, nil
}

func (c *managedConn) SetReadDeadline(t time.Time) error {
	c.readDeadline.set(t)
	return c.Conn.SetReadDeadline(t)
}
func (c *managedConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline.set(t)
	return c.Conn.SetWriteDeadline(t)
}
func (c *managedConn) SetDeadline(t time.Time) error {
	c.readDeadline.set(t)
	c.writeDeadline.set(t)
	return c.Conn.SetDeadline(t)
}
