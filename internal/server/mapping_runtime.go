package server

import (
	"net"
	"sync"
	"sync/atomic"

	"hy2tun/internal/config"
	"hy2tun/internal/transport"
	"hy2tun/internal/view"
)

// mappingRuntime is the server-side state of one registered mapping. Traffic
// counters are atomic because the data-plane goroutines added by later tasks
// update them concurrently with read models.
type mappingRuntime struct {
	// id is the immutable mapping identifier. Data-plane goroutines may read it
	// without the manager lock; mapping itself is mutable and must stay under
	// that lock.
	id    string
	owner *clientSession // Immutable owner of this runtime, including pre-bound pushes.

	mapping      config.Mapping
	status       string
	errCode      string
	assignedPort int
	udpToken     string
	exposure     view.Exposure

	listeners *mappingListenerSet
	data      *dataStreams
	closed    atomic.Bool
	// acceptGen is incremented whenever the listener set is retired by an
	// exposure switch or close. Accept loops and accepted connections capture
	// it so a connection accepted by an old listener cannot be relayed after
	// the mapping stops listening on that address family.
	acceptGen atomic.Uint64
	closeOnce sync.Once

	// udpMu serializes publicUDPConn creation with the eviction of the
	// previous HY2 UDP session wrapper. udpSession is the wrapper whose read
	// loop currently owns the public socket; only one session may read it.
	udpMu      sync.Mutex
	udpSession *publicUDPConn

	activeMu        sync.Mutex
	active          map[net.Conn]struct{}
	activeDatagrams map[transport.DatagramConn]struct{}
	connSem         chan struct{}

	txBytes  atomic.Uint64
	rxBytes  atomic.Uint64
	udpDrops atomic.Uint64
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

func (m *mappingRuntime) trackDatagram(conn transport.DatagramConn) {
	m.activeMu.Lock()
	if m.activeDatagrams == nil {
		m.activeDatagrams = make(map[transport.DatagramConn]struct{})
	}
	m.activeDatagrams[conn] = struct{}{}
	m.activeMu.Unlock()
}

func (m *mappingRuntime) untrackDatagram(conn transport.DatagramConn) {
	m.activeMu.Lock()
	delete(m.activeDatagrams, conn)
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
	datagrams := make([]transport.DatagramConn, 0, len(m.activeDatagrams))
	for conn := range m.activeDatagrams {
		datagrams = append(datagrams, conn)
	}
	m.activeDatagrams = nil
	m.activeMu.Unlock()
	// Datagram Close can wait for a managed stream's FIN/reset. Never impose
	// child draining on a manager-held update/remove/revoke operation.
	if len(datagrams) > 0 {
		go func() {
			for _, conn := range datagrams {
				_ = conn.Close()
			}
		}()
	}

	for _, conn := range conns {
		_ = conn.Close()
	}
}

// close releases the listener set, pending data streams, and active
// connections owned by the runtime. The public UDP sockets are owned by the
// runtime and are closed here exactly once.
func (m *mappingRuntime) close() {
	m.closeOnce.Do(func() {
		// Retire the current accept generation before closing the listeners so
		// an accepted-but-not-yet-tracked connection cannot be relayed.
		m.acceptGen.Add(1)
		m.closed.Store(true)
		if m.listeners != nil {
			_ = m.listeners.Close()
			if m.listeners.UDP != nil {
				// Closing the owned sockets releases the public port immediately.
				// The child reader/bridge drain, including a concurrent attachment,
				// is completed outside the shared manager lock.
				go func() {
					m.udpMu.Lock()
					public := m.udpSession
					m.udpSession = nil
					m.udpMu.Unlock()
					if public != nil {
						_ = public.Close()
					}
				}()
			}
		}
		m.closeActive()
		if m.data != nil {
			m.data.dropMapping(m)
		}
	})
}

// clientSession is one live control stream and its registered mappings.
