package client

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"

	"hy2tun/internal/config"
)

const (
	// udpVirtualPrefix is the reserved virtual host prefix for reverse UDP
	// sessions.
	udpVirtualPrefix = "_hy2tun_udp_"
	// udpKeepaliveInterval must stay well below the HY2 server UDP idle
	// timeout (60 seconds by default).
	udpKeepaliveInterval = 20 * time.Second
	// udpPeerIdleTimeout is how long an idle public peer keeps its local
	// socket.
	udpPeerIdleTimeout = 60 * time.Second
	// udpPeerCleanupInterval controls how often idle peers are reaped.
	udpPeerCleanupInterval = 10 * time.Second
	// udpMaxPeers bounds the number of local sockets one mapping may own.
	udpMaxPeers = 1024
	// udpMaxDatagram is the HY2 UDP payload limit. Larger datagrams are
	// dropped with a warning instead of being handed to the core.
	udpMaxDatagram = 4096
	// udpReadBufferSize is large enough for any IPv4/IPv6 UDP datagram.
	udpReadBufferSize = 64 * 1024
)

// udpVirtualAddress returns the virtual address that carries one reverse UDP
// mapping. The token is never logged.
func udpVirtualAddress(token string) string {
	return udpVirtualPrefix + token + ":0"
}

// udpMapping owns one HY2 UDP session and one local UDP socket per public peer
// for a single reverse UDP mapping.
type udpMapping struct {
	id          string
	virtualAddr string
	conn        hyclient.HyUDPConn
	log         *slog.Logger

	cancel context.CancelFunc

	mu     sync.Mutex
	peers  map[string]*udpPeer
	closed bool

	// sendMu serializes HyUDPConn.Send, which is not thread-safe because the
	// core reuses one send buffer.
	sendMu sync.Mutex

	target            *net.UDPAddr
	maxPeers          int
	idleTimeout       time.Duration
	keepaliveInterval time.Duration
	cleanupInterval   time.Duration

	wg        sync.WaitGroup
	closeOnce sync.Once
}

// udpPeer is the local socket that talks to the mapping's target on behalf of
// one public peer address.
type udpPeer struct {
	addr      string
	conn      *net.UDPConn
	last      atomic.Int64
	closeOnce sync.Once
}

func (p *udpPeer) touch() {
	p.last.Store(time.Now().UnixNano())
}

func (p *udpPeer) lastSeen() time.Time {
	return time.Unix(0, p.last.Load())
}

func (p *udpPeer) close() {
	p.closeOnce.Do(func() { _ = p.conn.Close() })
}

// startUDP opens one HY2 UDP session for m, sends the virtual-address probe,
// and starts the receive, keepalive, and peer-cleanup loops. The caller owns the
// returned mapping and must close it.
func (c *Client) startUDP(m config.Mapping, token string) (*udpMapping, error) {
	if !m.Enabled || m.Type != "udp" {
		return nil, fmt.Errorf("mapping %q is not an enabled UDP mapping", m.ID)
	}
	if token == "" {
		return nil, fmt.Errorf("mapping %q has no UDP token", m.ID)
	}
	if !validDataToken(token) {
		return nil, fmt.Errorf("mapping %q has a malformed UDP token", m.ID)
	}
	hy := c.hyClient()
	if hy == nil {
		return nil, fmt.Errorf("HY2 client is not connected")
	}
	target, err := net.ResolveUDPAddr("udp", m.LocalAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve UDP target %q: %w", m.LocalAddr, err)
	}
	conn, err := hy.UDP()
	if err != nil {
		return nil, fmt.Errorf("open HY2 UDP session: %w", err)
	}
	if conn == nil {
		return nil, fmt.Errorf("open HY2 UDP session: nil connection")
	}

	ctx, cancel := context.WithCancel(context.Background())
	u := &udpMapping{
		id:                m.ID,
		virtualAddr:       udpVirtualAddress(token),
		conn:              conn,
		log:               c.log,
		cancel:            cancel,
		peers:             make(map[string]*udpPeer),
		target:            target,
		maxPeers:          udpMaxPeers,
		idleTimeout:       udpPeerIdleTimeout,
		keepaliveInterval: udpKeepaliveInterval,
		cleanupInterval:   udpPeerCleanupInterval,
	}
	if u.log == nil {
		u.log = slog.Default()
	}

	// The probe creates the server-side UDP session and binds it to this
	// mapping's public socket. It carries no payload.
	if err := u.send([]byte{0}, u.virtualAddr); err != nil {
		cancel()
		_ = conn.Close()
		return nil, fmt.Errorf("send reverse UDP probe: %w", err)
	}

	u.wg.Add(3)
	go func() {
		defer u.wg.Done()
		u.receiveLoop(ctx)
	}()
	go func() {
		defer u.wg.Done()
		u.keepalive(ctx)
	}()
	go func() {
		defer u.wg.Done()
		u.cleanupPeers(ctx)
	}()
	return u, nil
}

// startUDPMappings starts one HY2 UDP session per enabled UDP mapping that the
// server registered successfully. Failures are logged and do not prevent other
// mappings from starting.
func (c *Client) startUDPMappings() {
	c.mu.Lock()
	mappings := append([]config.Mapping(nil), c.cfg.Mappings...)
	tokens := make(map[string]string, len(c.udpTokens))
	for id, token := range c.udpTokens {
		tokens[id] = token
	}
	c.mu.Unlock()

	for _, mapping := range mappings {
		if c.isClosed() {
			return
		}
		if mapping.Type != "udp" || !mapping.Enabled {
			continue
		}
		token := tokens[mapping.ID]
		if token == "" {
			c.log.Warn("reverse UDP mapping has no server token", "mapping", mapping.ID)
			continue
		}
		u, err := c.startUDP(mapping, token)
		if err != nil {
			c.log.Warn("start reverse UDP mapping", "mapping", mapping.ID, "error", err)
			continue
		}

		c.mu.Lock()
		if c.isClosed() {
			c.mu.Unlock()
			u.close()
			continue
		}
		old := c.udpMappings[mapping.ID]
		if c.udpMappings == nil {
			c.udpMappings = make(map[string]*udpMapping)
		}
		c.udpMappings[mapping.ID] = u
		c.mu.Unlock()
		if old != nil {
			old.close()
		}
	}
}

// stopUDPMappings closes every live reverse UDP session. It is idempotent and
// safe to call concurrently with startUDPMappings.
func (c *Client) stopUDPMappings() {
	c.mu.Lock()
	mappings := c.udpMappings
	c.udpMappings = make(map[string]*udpMapping)
	c.mu.Unlock()

	for _, u := range mappings {
		if u != nil {
			u.close()
		}
	}
}

// receiveLoop forwards datagrams from the server to the local socket that
// belongs to the public peer address.
func (u *udpMapping) receiveLoop(ctx context.Context) {
	for {
		data, peerAddr, err := u.conn.Receive()
		if err != nil {
			if ctx.Err() == nil && !u.isClosed() {
				u.logger().Debug("reverse UDP receive failed", "mapping", u.id, "error", err)
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		if len(data) > udpMaxDatagram {
			u.logger().Warn("dropping oversized reverse UDP datagram",
				"mapping", u.id,
				"size", len(data),
				"limit", udpMaxDatagram,
			)
			continue
		}
		// The virtual address only carries probes/keepalives and must never be
		// treated as a public peer.
		if peerAddr == "" || peerAddr == u.virtualAddr {
			continue
		}

		peer, err := u.peer(peerAddr)
		if err != nil {
			if !u.isClosed() {
				u.logger().Debug("create reverse UDP peer socket",
					"mapping", u.id,
					"peer", peerAddr,
					"error", err,
				)
			}
			continue
		}
		if _, err := peer.conn.Write(data); err != nil {
			u.logger().Debug("write reverse UDP peer",
				"mapping", u.id,
				"peer", peerAddr,
				"error", err,
			)
			u.removePeer(peer)
			continue
		}
		peer.touch()
	}
}

// peer returns the local socket for a public peer address, creating it on first
// use. Each peer gets its own socket so replies cannot be delivered to another
// public peer.
func (u *udpMapping) peer(addr string) (*udpPeer, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil, net.ErrClosed
	}
	if p := u.peers[addr]; p != nil {
		p.touch()
		return p, nil
	}

	limit := u.maxPeers
	if limit <= 0 {
		limit = udpMaxPeers
	}
	if len(u.peers) >= limit {
		if oldest := u.oldestPeerLocked(); oldest != nil {
			delete(u.peers, oldest.addr)
			oldest.close()
			u.logger().Warn("reverse UDP peer limit reached; dropped oldest peer",
				"mapping", u.id,
				"peer", oldest.addr,
				"limit", limit,
			)
		}
	}

	conn, err := net.DialUDP("udp", nil, u.target)
	if err != nil {
		return nil, err
	}
	p := &udpPeer{addr: addr, conn: conn}
	p.touch()
	u.peers[addr] = p
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.replyLoop(p)
	}()
	return p, nil
}

// replyLoop forwards replies from the local target back to the public peer that
// owns this socket.
func (u *udpMapping) replyLoop(peer *udpPeer) {
	buf := make([]byte, udpReadBufferSize)
	for {
		n, err := peer.conn.Read(buf)
		if err != nil {
			u.removePeer(peer)
			return
		}
		if n > udpMaxDatagram {
			u.logger().Warn("dropping oversized local UDP reply",
				"mapping", u.id,
				"size", n,
				"limit", udpMaxDatagram,
			)
			continue
		}
		peer.touch()
		if n == 0 {
			// The HY2 UDPMessage format cannot represent an empty datagram.
			continue
		}
		if err := u.send(buf[:n], peer.addr); err != nil {
			if !u.isClosed() {
				u.logger().Debug("send reverse UDP reply",
					"mapping", u.id,
					"peer", peer.addr,
					"error", err,
				)
			}
			u.removePeer(peer)
			return
		}
	}
}

// keepalive refreshes the server-side UDP session. The server drops the packet
// but updates the session's idle timestamp.
func (u *udpMapping) keepalive(ctx context.Context) {
	interval := u.keepaliveInterval
	if interval <= 0 {
		interval = udpKeepaliveInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := u.send([]byte{0}, u.virtualAddr); err != nil {
				if ctx.Err() != nil || u.isClosed() {
					return
				}
				// A transient send failure must not stop future keepalives:
				// without them the server would reap the UDP session.
				u.logger().Debug("send reverse UDP keepalive", "mapping", u.id, "error", err)
			}
		}
	}
}

// cleanupPeers periodically closes local sockets whose public peer has been
// idle for the configured timeout.
func (u *udpMapping) cleanupPeers(ctx context.Context) {
	interval := u.cleanupInterval
	if interval <= 0 {
		interval = udpPeerCleanupInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			u.expirePeers(now)
		}
	}
}

func (u *udpMapping) expirePeers(now time.Time) {
	timeout := u.idleTimeout
	if timeout <= 0 {
		timeout = udpPeerIdleTimeout
	}

	u.mu.Lock()
	var expired []*udpPeer
	for _, p := range u.peers {
		if now.Sub(p.lastSeen()) > timeout {
			delete(u.peers, p.addr)
			expired = append(expired, p)
		}
	}
	u.mu.Unlock()

	for _, p := range expired {
		p.close()
		u.logger().Debug("closed idle reverse UDP peer", "mapping", u.id, "peer", p.addr)
	}
}

func (u *udpMapping) oldestPeerLocked() *udpPeer {
	var oldest *udpPeer
	for _, p := range u.peers {
		if oldest == nil || p.lastSeen().Before(oldest.lastSeen()) {
			oldest = p
		}
	}
	return oldest
}

func (u *udpMapping) removePeer(peer *udpPeer) {
	u.mu.Lock()
	if u.peers[peer.addr] == peer {
		delete(u.peers, peer.addr)
	}
	u.mu.Unlock()
	peer.close()
}

// send serializes every HyUDPConn.Send call and drops datagrams that exceed the
// HY2 payload limit instead of handing them to the core.
func (u *udpMapping) send(data []byte, addr string) error {
	if len(data) > udpMaxDatagram {
		u.logger().Warn("dropping oversized reverse UDP datagram",
			"mapping", u.id,
			"size", len(data),
			"limit", udpMaxDatagram,
		)
		return nil
	}

	u.sendMu.Lock()
	defer u.sendMu.Unlock()
	if u.conn == nil || u.isClosed() {
		return net.ErrClosed
	}
	return u.conn.Send(data, addr)
}

// close stops the loops, closes every local peer socket, and closes the HY2 UDP
// session. It is idempotent.
func (u *udpMapping) close() {
	u.closeOnce.Do(func() {
		if u.cancel != nil {
			u.cancel()
		}

		u.mu.Lock()
		u.closed = true
		peers := make([]*udpPeer, 0, len(u.peers))
		for _, p := range u.peers {
			peers = append(peers, p)
		}
		u.peers = make(map[string]*udpPeer)
		u.mu.Unlock()

		for _, p := range peers {
			p.close()
		}
		if u.conn != nil {
			_ = u.conn.Close()
		}
		u.wg.Wait()
	})
}

func (u *udpMapping) isClosed() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.closed
}

func (u *udpMapping) peerCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.peers)
}

func (u *udpMapping) logger() *slog.Logger {
	if u.log != nil {
		return u.log
	}
	return slog.Default()
}
