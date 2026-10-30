package server

import (
	"net"
	"sync"
	"sync/atomic"

	"hy2tun/internal/config"
)

// mappingRuntime is the server-side state of one registered mapping. Traffic
// counters are atomic because the data-plane goroutines added by later tasks
// update them concurrently with read models.
type mappingRuntime struct {
	// id is the immutable mapping identifier. Data-plane goroutines may read it
	// without the manager lock; mapping itself is mutable and must stay under
	// that lock.
	id string

	mapping      config.Mapping
	status       string
	errCode      string
	assignedPort int
	udpToken     string

	tcpListener net.Listener
	udpConn     *net.UDPConn
	data        *dataStreams
	closed      atomic.Bool
	closeOnce   sync.Once

	// udpMu serializes publicUDPConn creation with the eviction of the
	// previous HY2 UDP session wrapper. udpSession is the wrapper whose read
	// loop currently owns the public socket; only one session may read it.
	udpMu      sync.Mutex
	udpSession *publicUDPConn

	activeMu sync.Mutex
	active   map[net.Conn]struct{}
	connSem  chan struct{}

	txBytes atomic.Uint64
	rxBytes atomic.Uint64
}

// trackActive records a live data-plane connection for this mapping.
func (m *mappingRuntime) trackActive(conn net.Conn) {
	if conn == nil {
		return
	}
	m.activeMu.Lock()
	if m.active == nil {
		m.active = make(map[net.Conn]struct{})
	}
	m.active[conn] = struct{}{}
	m.activeMu.Unlock()
}

// untrackActive removes a connection from the active set.
func (m *mappingRuntime) untrackActive(conn net.Conn) {
	m.activeMu.Lock()
	delete(m.active, conn)
	m.activeMu.Unlock()
}

// closeActive closes every live connection for this mapping. It is used when a
// mapping is removed, disabled, or retargeted.
func (m *mappingRuntime) closeActive() {
	m.activeMu.Lock()
	conns := make([]net.Conn, 0, len(m.active))
	for conn := range m.active {
		conns = append(conns, conn)
	}
	m.active = make(map[net.Conn]struct{})
	m.activeMu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

// close releases the listener, UDP socket, pending data streams, and active
// connections owned by the runtime. The public UDP socket is owned by the
// runtime and is closed here exactly once.
func (m *mappingRuntime) close() {
	m.closeOnce.Do(func() {
		m.closed.Store(true)
		if m.tcpListener != nil {
			_ = m.tcpListener.Close()
		}
		if m.udpConn != nil {
			_ = m.udpConn.Close()
		}
		m.closeActive()
		if m.data != nil {
			m.data.dropMapping(m)
		}
	})
}

// clientSession is one live control stream and its registered mappings.
