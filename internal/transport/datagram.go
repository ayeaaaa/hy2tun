package transport

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
)

const (
	maxDatagramPayload = 4096
	maxDatagramAddr    = 256
	maxDatagramFrame   = 2 + maxDatagramAddr + maxDatagramPayload
	datagramQueueSize  = 32
)

// Datagram lengths are checked independently before body allocation. frameLen
// includes uint16 addrLen, but excludes the uint32 frameLen prefix.
func ReadDatagramFrame(r io.Reader) ([]byte, string, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, "", err
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n < 2 || n > maxDatagramFrame {
		return nil, "", ErrProtocol
	}
	var addrPrefix [2]byte
	if _, err := io.ReadFull(r, addrPrefix[:]); err != nil {
		return nil, "", err
	}
	a := uint32(binary.BigEndian.Uint16(addrPrefix[:]))
	if a > maxDatagramAddr || a > n-2 || n-2-a > maxDatagramPayload {
		return nil, "", ErrProtocol
	}
	body := make([]byte, n-2)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, "", err
	}
	return body[a:], string(body[:a]), nil
}

func WriteDatagramFrame(w io.Writer, payload []byte, addr string) error {
	if len(payload) > maxDatagramPayload || len(addr) > maxDatagramAddr {
		return ErrProtocol
	}
	buf := make([]byte, 6+len(addr)+len(payload))
	binary.BigEndian.PutUint32(buf[:4], uint32(2+len(addr)+len(payload)))
	binary.BigEndian.PutUint16(buf[4:6], uint16(len(addr)))
	copy(buf[6:], addr)
	copy(buf[6+len(addr):], payload)
	return writeFull(w, buf)
}

type queuedDatagram struct {
	payload []byte
	addr    string
}
type tcpDatagram struct {
	conn     net.Conn
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	queue    chan queuedDatagram
	closed   bool
	closeErr error
	readMu   sync.Mutex
}

// NewDatagramConn takes ownership of an acknowledged UDP stream. The accepted
// TCPPeer stream already carries session-wide server rate limits. Close drops
// queued packets; it never replays them onto a replacement mapping.
func NewDatagramConn(conn net.Conn) DatagramConn {
	parent := context.Background()
	if c, ok := conn.(*managedConn); ok {
		parent = c.ctx
	}
	ctx, cancel := context.WithCancel(parent)
	d := &tcpDatagram{conn: conn, ctx: ctx, cancel: cancel, queue: make(chan queuedDatagram, datagramQueueSize)}
	context.AfterFunc(ctx, func() { _ = d.Close() })
	go d.sendLoop()
	return d
}

// QueueCapacity reports the fixed bound of the datagram send queue. Callers
// can discover it through an optional interface without widening the
// DatagramConn contract.
func (d *tcpDatagram) QueueCapacity() int {
	return cap(d.queue)
}

func (d *tcpDatagram) Send(payload []byte, addr string) error {
	if len(payload) > maxDatagramPayload || len(addr) > maxDatagramAddr {
		return ErrProtocol
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.ctx.Err() != nil {
		return net.ErrClosed
	}
	if len(d.queue) == cap(d.queue) {
		return ErrQueueFull
	}
	d.queue <- queuedDatagram{payload: append([]byte(nil), payload...), addr: addr}
	return nil
}

func (d *tcpDatagram) sendLoop() {
	for {
		select {
		case <-d.ctx.Done():
			return
		case frame := <-d.queue:
			if d.ctx.Err() != nil {
				return
			}
			if err := WriteDatagramFrame(d.conn, frame.payload, frame.addr); err != nil {
				_ = d.Close()
				return
			}
		}
	}
}

func (d *tcpDatagram) Receive() ([]byte, string, error) {
	d.readMu.Lock()
	defer d.readMu.Unlock()
	if d.ctx.Err() != nil {
		return nil, "", net.ErrClosed
	}
	p, addr, err := ReadDatagramFrame(d.conn)
	if err != nil {
		_ = d.Close()
	}
	return p, addr, err
}

func (d *tcpDatagram) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return d.closeErr
	}
	d.closed = true
	d.cancel()
	for {
		select {
		case <-d.queue:
		default:
			d.closeErr = d.conn.Close()
			return d.closeErr
		}
	}
}
