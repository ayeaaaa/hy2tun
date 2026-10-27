package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"time"

	"hy2tun/internal/control"
)

const (
	// dataTokenTTL bounds how long a public connection waits for the client to
	// attach its reverse data stream.
	dataTokenTTL = 10 * time.Second
	// dataTokenJanitorInterval controls how often expired pending connections
	// are reaped.
	dataTokenJanitorInterval = time.Second
)

// pendingConn is one public connection waiting for the reverse data stream
// identified by its one-time token.
type pendingConn struct {
	conn    net.Conn
	mapping *mappingRuntime
	expires time.Time
}

// dataStreams maps one-time 128-bit tokens to public connections that are
// waiting for the owning client to open the matching data stream. Tokens are
// single-use and expire after dataTokenTTL.
type dataStreams struct {
	mu      sync.Mutex
	pending map[string]*pendingConn

	stop     chan struct{}
	stopOnce sync.Once
}

func newDataStreams() *dataStreams {
	return &dataStreams{
		pending: make(map[string]*pendingConn),
		stop:    make(chan struct{}),
	}
}

// add registers pc under a fresh random token and returns the token.
func (d *dataStreams) add(pc *pendingConn) (string, error) {
	if pc == nil || pc.conn == nil {
		return "", fmt.Errorf("register data stream: missing connection")
	}
	for attempt := 0; attempt < 4; attempt++ {
		token, err := newDataToken()
		if err != nil {
			return "", err
		}

		d.mu.Lock()
		_, exists := d.pending[token]
		if !exists {
			d.pending[token] = pc
		}
		d.mu.Unlock()
		if !exists {
			return token, nil
		}
	}
	return "", fmt.Errorf("register data stream: could not allocate a unique token")
}

// take consumes a token and returns its public connection. A token can be
// claimed at most once, and only before it expires.
func (d *dataStreams) take(token string) (net.Conn, error) {
	d.mu.Lock()
	pc := d.pending[token]
	if pc != nil {
		delete(d.pending, token)
	}
	d.mu.Unlock()

	if pc == nil {
		return nil, fmt.Errorf("hy2tun data stream token is unknown")
	}
	if time.Now().After(pc.expires) {
		_ = pc.conn.Close()
		return nil, fmt.Errorf("hy2tun data stream token has expired")
	}
	if pc.mapping != nil && pc.mapping.closed.Load() {
		_ = pc.conn.Close()
		return nil, fmt.Errorf("hy2tun data stream mapping is closed")
	}
	return pc.conn, nil
}

// remove drops a token and closes its public connection. It is used when the
// open request cannot be delivered.
func (d *dataStreams) remove(token string) {
	d.mu.Lock()
	pc := d.pending[token]
	delete(d.pending, token)
	d.mu.Unlock()

	if pc != nil {
		_ = pc.conn.Close()
	}
}

// fail drops a token after the client reported that it could not open the
// stream, so the public peer does not wait for the full token TTL.
func (d *dataStreams) fail(token string) {
	d.remove(token)
}

// dropMapping closes every pending connection owned by m.
func (d *dataStreams) dropMapping(m *mappingRuntime) {
	if m == nil {
		return
	}
	d.mu.Lock()
	conns := make([]net.Conn, 0, 1)
	for token, pc := range d.pending {
		if pc.mapping != m {
			continue
		}
		delete(d.pending, token)
		conns = append(conns, pc.conn)
	}
	d.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

// expire closes and removes pending connections whose token has expired.
func (d *dataStreams) expire(now time.Time) {
	d.mu.Lock()
	conns := make([]net.Conn, 0, 1)
	for token, pc := range d.pending {
		if now.Before(pc.expires) {
			continue
		}
		delete(d.pending, token)
		conns = append(conns, pc.conn)
	}
	d.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

// janitor reaps expired pending connections until the server closes.
func (d *dataStreams) janitor() {
	ticker := time.NewTicker(dataTokenJanitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-d.stop:
			return
		case now := <-ticker.C:
			d.expire(now)
		}
	}
}

// closeAll stops the janitor and closes every pending connection.
func (d *dataStreams) closeAll() {
	d.stopOnce.Do(func() { close(d.stop) })

	d.mu.Lock()
	conns := make([]net.Conn, 0, len(d.pending))
	for token, pc := range d.pending {
		delete(d.pending, token)
		conns = append(conns, pc.conn)
	}
	d.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

// newDataToken returns a 128-bit random hex token.
func newDataToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("generate data stream token: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}

// activeConn is a public connection tracked by its mapping runtime. Closing it
// removes it from the runtime's active set, so a completed relay does not leave
// a stale entry behind.
type activeConn struct {
	net.Conn
	mapping *mappingRuntime
	once    sync.Once
}

func (c *activeConn) Close() error {
	c.once.Do(func() {
		if c.mapping != nil {
			c.mapping.untrackActive(c)
		}
	})
	return c.Conn.Close()
}

// serveTCP accepts public connections for one reverse TCP mapping and asks the
// owning client to open a data stream for each of them.
func (s *Server) serveTCP(sess *clientSession, m *mappingRuntime, listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go s.openPublicConn(sess, m, conn)
	}
}

// openPublicConn hands one accepted public connection to the client. The
// connection is closed when no data stream can be opened for it.
func (s *Server) openPublicConn(sess *clientSession, m *mappingRuntime, conn net.Conn) {
	public := &activeConn{Conn: conn, mapping: m}
	m.trackActive(public)

	if err := s.openDataStream(sess, m, public); err != nil {
		s.log.Debug("reverse TCP open failed",
			"mapping", m.id,
			"error", err,
		)
		_ = public.Close()
	}
}

// openDataStream allocates a one-time token for public and sends an open
// request over the mapping owner's control stream.
func (s *Server) openDataStream(sess *clientSession, m *mappingRuntime, public net.Conn) error {
	if sess == nil || m == nil || public == nil {
		return fmt.Errorf("open data stream: missing session, mapping, or connection")
	}
	if s.data == nil {
		return fmt.Errorf("open data stream: data plane is not initialized")
	}
	if err := sess.waitReady(dataTokenTTL); err != nil {
		return fmt.Errorf("open data stream: %w", err)
	}
	if m.closed.Load() {
		return fmt.Errorf("open data stream: mapping is closed")
	}

	token, err := s.data.add(&pendingConn{
		conn:    public,
		mapping: m,
		expires: time.Now().Add(dataTokenTTL),
	})
	if err != nil {
		return err
	}
	if err := sess.write(&control.Open{
		Type:      control.TypeOpen,
		Token:     token,
		MappingID: m.id,
	}); err != nil {
		s.data.remove(token)
		return fmt.Errorf("send control open: %w", err)
	}
	return nil
}

// removeMapping removes one mapping from a live client, closing its listener
// and any traffic it is carrying.
func (s *Server) removeMapping(clientID, mappingID string) error {
	return s.mappings.removeMapping(clientID, mappingID)
}
