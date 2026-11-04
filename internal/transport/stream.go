package transport

import (
	"context"
	"errors"
	"net"
	"sync"
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

func (s *tcpSession) takeSlot(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ctx.Err() != nil {
		return net.ErrClosed
	}
	if s.mux.NumStreams() >= maxTCPStreams {
		return ErrQueueFull
	}
	select {
	case s.slots <- struct{}{}:
		return nil
	default:
		return ErrQueueFull
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
	if err := s.takeSlot(opctx); err != nil {
		return nil, err
	}
	type result struct {
		c   *managedConn
		err error
	}
	results := make(chan result)
	go func() {
		raw, err := s.mux.OpenStream()
		if err != nil {
			<-s.slots
		} else {
			c := s.manage(raw, h.Kind)
			stop := context.AfterFunc(opctx, func() { _ = c.abort() })
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
				err = net.ErrClosed
			}
			if err == nil {
				_ = raw.SetDeadline(time.Time{})
			}
			if err != nil {
				_ = c.abort()
			} else {
				select {
				case results <- result{c: c}:
					return
				case <-opctx.Done():
				case <-s.ctx.Done():
				}
				_ = c.abort()
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
		return nil, net.ErrClosed
	case r := <-results:
		if err := opctx.Err(); err != nil {
			if r.c != nil {
				_ = r.c.abort()
			}
			return nil, err
		}
		if s.ctx.Err() != nil {
			if r.c != nil {
				_ = r.c.abort()
			}
			return nil, net.ErrClosed
		}
		return r.c, r.err
	}
}

func (p *TCPPeer) Accept(ctx context.Context) (StreamHeader, net.Conn, error) {
	s := p.session
	if err := s.takeSlot(ctx); err != nil {
		return StreamHeader{}, nil, err
	}
	raw, err := s.mux.AcceptStreamWithContext(ctx)
	if err != nil {
		<-s.slots
		return StreamHeader{}, nil, contextError(ctx, err)
	}
	opctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	stop := context.AfterFunc(opctx, func() { _ = raw.Reset() })
	deadline, _ := opctx.Deadline()
	_ = raw.SetDeadline(deadline)
	var h StreamHeader
	err = readJSONFrame(raw, &h, streamFrameLimit)
	if err == nil && !validStreamHeader(h) {
		err = ErrProtocol
	}
	if err != nil {
		if errors.Is(err, ErrProtocol) {
			_ = WriteStreamAck(raw, ErrProtocol)
		}
		stop()
		_ = raw.Close()
		<-s.slots
		return StreamHeader{}, nil, contextError(opctx, err)
	}
	if !stop() || opctx.Err() != nil {
		_ = raw.Reset()
		<-s.slots
		return StreamHeader{}, nil, contextError(opctx, net.ErrClosed)
	}
	_ = raw.SetReadDeadline(time.Time{})
	// Keep the write deadline until the upper layer sends its application ack.
	return h, s.manage(raw, h.Kind), nil
}

type managedConn struct {
	net.Conn
	ctx                         context.Context
	cancel                      context.CancelFunc
	readLimit, writeLimit       *byteLimiter
	closeOnce                   sync.Once
	closeErr                    error
	release                     func()
	readDeadline, writeDeadline deadlineState
}

func (s *tcpSession) manage(raw *yamux.Stream, kind uint8) *managedConn {
	ctx, cancel := context.WithCancel(s.ctx)
	c := &managedConn{Conn: raw, ctx: ctx, cancel: cancel, release: func() { <-s.slots }}
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
		if c.release != nil {
			c.release()
		}
		if reset {
			c.closeErr = c.Conn.(*yamux.Stream).Reset()
		} else {
			// Close is graceful for already-written bytes, but releases local reads
			// like net.Conn.Close rather than yamux's half-close semantics.
			_ = c.Conn.SetReadDeadline(time.Now())
			c.closeErr = c.Conn.Close()
		}
	})
	return c.closeErr
}
func (c *managedConn) Close() error { return c.shutdown(false) }
func (c *managedConn) abort() error { return c.shutdown(true) }

func (c *managedConn) Read(p []byte) (int, error) {
	if c.ctx.Err() != nil {
		return 0, net.ErrClosed
	}
	if c.readLimit == nil {
		return c.Conn.Read(p)
	}
	if len(p) > rateChunk {
		p = p[:rateChunk]
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		if e := c.readLimit.waitDeadline(c.ctx, n, &c.readDeadline); e != nil {
			return 0, e
		}
	}
	return n, err
}

func (c *managedConn) Write(p []byte) (int, error) {
	if c.ctx.Err() != nil {
		return 0, net.ErrClosed
	}
	if c.writeLimit == nil {
		return c.Conn.Write(p)
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > rateChunk {
			n = rateChunk
		}
		if err := c.writeLimit.waitDeadline(c.ctx, n, &c.writeDeadline); err != nil {
			return total, err
		}
		written, err := c.Conn.Write(p[:n])
		total += written
		p = p[written:]
		if err != nil {
			return total, err
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
