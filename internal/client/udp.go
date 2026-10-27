package client

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	// udpMessageBufferSize is the HY2 UDPMessage buffer size. It bounds the
	// whole message (fixed header + address + payload), not just the payload.
	udpMessageBufferSize = 4096
	// udpReadBufferSize is large enough for any IPv4/IPv6 UDP datagram.
	udpReadBufferSize = 64 * 1024
)

// udpPayloadLimit returns the largest payload that fits in one HY2 UDPMessage
// for addr: an 8-byte fixed header, a two-byte address-length varint, the
// address itself, and the payload. Two bytes cover every realistic peer
// address; a longer address simply leaves a little space unused.
func udpPayloadLimit(addr string) int {
	limit := udpMessageBufferSize - 8 - 2 - len(addr)
	if limit < 1 {
		return 1
	}
	return limit
}

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
	hy          hyclient.Client
	client      *Client
	log         *slog.Logger
	traffic     *mappingTraffic

	cancel context.CancelFunc

	mu     sync.Mutex
	peers  map[string]*udpPeer
	closed bool

	// sendMu serializes HyUDPConn.Send, which is not thread-safe because the
	// core reuses one send buffer.
	sendMu sync.Mutex

	target            *net.UDPAddr
	targetAddr        string
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
		hy:                hy,
		client:            c,
		log:               c.log,
		traffic:           c.trafficFor(m.ID),
		cancel:            cancel,
		peers:             make(map[string]*udpPeer),
		target:            target,
		targetAddr:        m.LocalAddr,
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

// startUDPMappings reconciles UDP sessions with the current configuration.
func (c *Client) startUDPMappings() {
	c.reconcileUDPMappings()
}

// reconcileUDPMappings starts, stops, and restarts reverse UDP sessions so
// they match the current configuration and server tokens. A mapping keeps its
// session when its target and token are unchanged; a token or local_addr
// change replaces it.
func (c *Client) reconcileUDPMappings() {
	c.udpReconcileMu.Lock()
	defer c.udpReconcileMu.Unlock()

	c.configMu.Lock()
	mappings := append([]config.Mapping(nil), c.cfg.Mappings...)
	c.configMu.Unlock()

	c.mu.Lock()
	tokens := make(map[string]string, len(c.udpTokens))
	for id, token := range c.udpTokens {
		tokens[id] = token
	}
	current := make(map[string]*udpMapping, len(c.udpMappings))
	for id, mapping := range c.udpMappings {
		current[id] = mapping
	}
	c.mu.Unlock()

	desired := make(map[string]config.Mapping, len(mappings))
	for _, mapping := range mappings {
		if mapping.Enabled && mapping.Type == "udp" {
			desired[mapping.ID] = mapping
		}
	}

	for id, existing := range current {
		mapping, ok := desired[id]
		token := tokens[id]
		if ok &&
			token != "" &&
			existing.virtualAddr == udpVirtualAddress(token) &&
			existing.targetAddr == mapping.LocalAddr {
			continue
		}
		existing.close()
		c.mu.Lock()
		if c.udpMappings[id] == existing {
			delete(c.udpMappings, id)
		}
		c.mu.Unlock()
	}

	for _, mapping := range mappings {
		if mapping.Type != "udp" || !mapping.Enabled {
			continue
		}
		token := tokens[mapping.ID]
		if token == "" {
			c.log.Debug("reverse UDP mapping is waiting for a server token", "mapping", mapping.ID)
			continue
		}

		c.mu.Lock()
		existing := c.udpMappings[mapping.ID]
		c.mu.Unlock()
		if existing != nil {
			continue
		}
		if c.isClosed() {
			return
		}

		u, err := c.startUDP(mapping, token)
		if err != nil {
			c.log.Warn("start reverse UDP mapping", "mapping", mapping.ID, "error", err)
			continue
		}

		c.mu.Lock()
		if c.isClosed() || c.hy != u.hy {
			c.mu.Unlock()
			u.close()
			continue
		}
		old := c.udpMappings[mapping.ID]
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
// belongs to the public peer address. An unexpected Receive error means this
// HY2 UDP session is dead; the mapping stays registered in the read model, so
// the control stream is closed to force a full reconnect and resync instead of
// leaving a running but deaf mapping.
func (u *udpMapping) receiveLoop(ctx context.Context) {
	for {
		data, peerAddr, err := u.conn.Receive()
		if err != nil {
			if ctx.Err() != nil || u.isClosed() || (u.client != nil && u.client.isClosed()) {
				return
			}
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				// Expected while a control stream is being torn down.
				u.logger().Debug("reverse UDP receive closed; reconnecting control stream",
					"mapping", u.id,
				)
			} else {
				u.logger().Warn("reverse UDP receive failed; reconnecting control stream",
					"mapping", u.id,
					"error", err,
				)
			}
			if u.client != nil {
				u.client.forceControlResync()
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		if len(data) > udpMessageBufferSize {
			u.logger().Warn("dropping oversized reverse UDP datagram",
				"mapping", u.id,
				"size", len(data),
				"limit", udpMessageBufferSize,
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
		if u.traffic != nil {
			u.traffic.rx.Add(uint64(len(data)))
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
		if victim := u.evictPeerLocked(time.Now()); victim != nil {
			delete(u.peers, victim.addr)
			victim.close()
			u.logger().Warn("reverse UDP peer limit reached; dropped peer",
				"mapping", u.id,
				"peer", victim.addr,
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
		if n > udpMessageBufferSize {
			u.logger().Warn("dropping oversized local UDP reply",
				"mapping", u.id,
				"size", n,
				"limit", udpMessageBufferSize,
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
		if u.traffic != nil {
			u.traffic.tx.Add(uint64(n))
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

// evictPeerLocked returns an idle peer if one exists, otherwise the least
// recently used peer. The caller must hold u.mu.
func (u *udpMapping) evictPeerLocked(now time.Time) *udpPeer {
	timeout := u.idleTimeout
	if timeout <= 0 {
		timeout = udpPeerIdleTimeout
	}
	var idle, oldest *udpPeer
	for _, p := range u.peers {
		last := p.lastSeen()
		if oldest == nil || last.Before(oldest.lastSeen()) {
			oldest = p
		}
		if now.Sub(last) > timeout && (idle == nil || last.Before(idle.lastSeen())) {
			idle = p
		}
	}
	if idle != nil {
		return idle
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

// send serializes every HyUDPConn.Send call and drops datagrams that do not
// fit in one HY2 UDPMessage (including its header and address) instead of
// handing them to the core.
func (u *udpMapping) send(data []byte, addr string) error {
	limit := udpPayloadLimit(addr)
	if len(data) > limit {
		u.logger().Warn("dropping reverse UDP datagram that exceeds the HY2 message buffer",
			"mapping", u.id,
			"size", len(data),
			"limit", limit,
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
