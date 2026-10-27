package client

import (
	"encoding/hex"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
)

const (
	// dataStreamPrefix is the reserved virtual host prefix for reverse data
	// streams.
	dataStreamPrefix = "_hy2tun_data_"
	// dataDialTimeout bounds how long a local target may take to accept a
	// reverse connection.
	dataDialTimeout = 10 * time.Second
	// dataTokenLength is the hex length of a 128-bit data stream token.
	dataTokenLength = 32
)

// dataStreamAddress returns the virtual address that carries the reverse data
// stream for token.
func dataStreamAddress(token string) string {
	return dataStreamPrefix + token + ":0"
}

// validDataToken reports whether token is a 128-bit hex value. Malformed tokens
// from a broken or hostile server are rejected before dialing anything.
func validDataToken(token string) bool {
	if len(token) != dataTokenLength {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

// handleOpen dials the local target for an incoming public connection and
// connects it to the matching reverse data stream. It runs outside the control
// read loop so a slow local dial cannot stall other messages.
func (c *Client) handleOpen(msg *control.Open) {
	if msg == nil {
		return
	}
	owner := c.controlStream()
	if owner == nil {
		return
	}
	if !validDataToken(msg.Token) {
		c.sendOpenResult(owner, msg.Token, false, "stream_failed")
		return
	}

	mapping, ok := c.tcpMapping(msg.MappingID)
	if !ok {
		c.sendOpenResult(owner, msg.Token, false, "mapping_missing")
		return
	}

	local, err := net.DialTimeout("tcp", mapping.LocalAddr, dataDialTimeout)
	if err != nil {
		c.sendOpenResult(owner, msg.Token, false, "dial_failed")
		return
	}

	hy := c.hyClient()
	if hy == nil {
		_ = local.Close()
		c.sendOpenResult(owner, msg.Token, false, "stream_failed")
		return
	}
	remote, err := hy.TCP(dataStreamAddress(msg.Token))
	if err != nil || remote == nil {
		if remote != nil {
			_ = remote.Close()
		}
		_ = local.Close()
		c.sendOpenResult(owner, msg.Token, false, "stream_failed")
		return
	}

	if !c.trackData(owner, local, remote) {
		// The control stream this open belonged to is already gone.
		_ = local.Close()
		_ = remote.Close()
		return
	}
	go func() {
		c.pipeMapping(msg.MappingID, local, remote)
		c.untrackData(local, remote)
	}()

	c.sendOpenResult(owner, msg.Token, true, "")
}

// pipe copies bytes in both directions until either side ends, then closes both
// connections and waits for the other copy to finish. Half-close is
// intentionally not used: the reverse stream ends as soon as either peer does.
func (c *Client) pipe(local, remote net.Conn) {
	c.pipeMapping("", local, remote)
}

// countingWriter increments a mapping counter for every byte written. It is
// deliberately a plain io.Writer so io.Copy cannot bypass it with ReaderFrom.
type countingWriter struct {
	writer  io.Writer
	counter *atomic.Uint64
}

func (w countingWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 && w.counter != nil {
		w.counter.Add(uint64(n))
	}
	return n, err
}

// pipeMapping is pipe with per-mapping traffic accounting. Bytes from the
// server to the local target count as down (Rx); bytes from the local target
// back to the server count as up (Tx). Counters advance on each successful
// write, so long-lived connections update while they are still open.
func (c *Client) pipeMapping(mappingID string, local, remote net.Conn) {
	traffic := c.trafficFor(mappingID)

	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = local.Close()
			_ = remote.Close()
		})
	}

	rxWriter := io.Writer(local)
	txWriter := io.Writer(remote)
	if traffic != nil {
		rxWriter = countingWriter{writer: local, counter: &traffic.rx}
		txWriter = countingWriter{writer: remote, counter: &traffic.tx}
	}

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(rxWriter, remote)
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(txWriter, local)
	}()

	<-done
	closeBoth()
	<-done
}

// controlStream returns the live control stream, if any.
func (c *Client) controlStream() net.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.controlConn
}

// hyClient returns the live HY2 client, if any.
func (c *Client) hyClient() hyclient.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hy == nil {
		return nil
	}
	return c.hy
}

// tcpMapping returns an enabled TCP mapping by ID.
func (c *Client) tcpMapping(id string) (config.Mapping, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	mapping, ok := c.mappingConfigs[id]
	if !ok || !mapping.Enabled || mapping.Type != "tcp" {
		return config.Mapping{}, false
	}
	return mapping, true
}

// trackData records data-plane connections under the control stream that owns
// them. It fails when owner is no longer the live stream, which closes the
// connection instead of leaking it across a reconnect.
func (c *Client) trackData(owner net.Conn, conns ...net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if owner == nil || c.controlConn != owner {
		return false
	}
	if c.dataConns == nil {
		c.dataConns = make(map[net.Conn]net.Conn)
	}
	for _, conn := range conns {
		if conn != nil {
			c.dataConns[conn] = owner
		}
	}
	return true
}

// untrackData forgets data-plane connections that have finished.
func (c *Client) untrackData(conns ...net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, conn := range conns {
		delete(c.dataConns, conn)
	}
}

// closeDataFor closes every data-plane connection owned by a control stream.
func (c *Client) closeDataFor(owner net.Conn) {
	c.mu.Lock()
	conns := make([]net.Conn, 0, 2)
	for conn, stream := range c.dataConns {
		if stream != owner {
			continue
		}
		delete(c.dataConns, conn)
		conns = append(conns, conn)
	}
	c.mu.Unlock()

	for _, conn := range conns {
		_ = conn.Close()
	}
}

// sendOpenResult reports the outcome of an open request. Tokens are never
// logged.
func (c *Client) sendOpenResult(owner net.Conn, token string, ok bool, code string) {
	if owner == nil {
		return
	}
	_ = c.writeControl(owner, &control.OpenResult{
		Type:  control.TypeOpenResult,
		Token: token,
		OK:    ok,
		Error: code,
	})
}
