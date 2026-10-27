// Package client implements the hy2tun client: an official Hysteria 2 client
// with a long-lived control stream used to register reverse port mappings.
package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	hyclient "github.com/apernet/hysteria/core/v2/client"
	"github.com/apernet/hysteria/extras/v2/obfs"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/view"
)

const (
	controlAddress = "_hy2tun_control_:0"

	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second
)

// Client is a hy2tun client. Start connects to the server and maintains the
// control stream until Close or context cancellation.
type Client struct {
	cfg *config.ClientConfig
	log *slog.Logger

	serverAddr  net.Addr
	tlsConfig   hyclient.TLSConfig
	connFactory hyclient.ConnFactory
	newHY2      func(*hyclient.Config) (hyclient.Client, *hyclient.HandshakeInfo, error)
	startedAt   time.Time

	mu             sync.Mutex
	hy             hyclient.Client
	controlConn    net.Conn
	connected      bool
	mappings       []view.MappingView
	mappingConfigs map[string]config.Mapping
	udpTokens      map[string]string
	udpMappings    map[string]*udpMapping
	// dataConns maps a live data-plane connection to the control stream that
	// owns it, so a dropped stream cannot leak local dials.
	dataConns map[net.Conn]net.Conn
	writeMu   sync.Mutex

	ready     chan struct{}
	readyOnce sync.Once
	closed    chan struct{}
	closeOnce sync.Once
	started   atomic.Bool
}

// New validates cfg and prepares the HY2 client configuration. It does not
// connect; call Start for that.
func New(cfg *config.ClientConfig, log *slog.Logger) (*Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("client config is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	if cfg.Client.ID == "" {
		id, err := newClientID()
		if err != nil {
			return nil, err
		}
		cfg.Client.ID = id
	}

	serverAddr, err := net.ResolveUDPAddr("udp", cfg.Server)
	if err != nil {
		return nil, fmt.Errorf("resolve server address %s: %w", cfg.Server, err)
	}
	tlsConfig, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	connFactory, err := buildConnFactory(cfg)
	if err != nil {
		return nil, err
	}

	return &Client{
		cfg:            cfg,
		log:            log,
		serverAddr:     serverAddr,
		tlsConfig:      tlsConfig,
		connFactory:    connFactory,
		newHY2:         hyclient.NewClient,
		startedAt:      time.Now(),
		mappings:       mappingViews(cfg.Mappings, nil),
		mappingConfigs: mappingConfigMap(cfg.Mappings),
		udpTokens:      make(map[string]string),
		udpMappings:    make(map[string]*udpMapping),
		dataConns:      make(map[net.Conn]net.Conn),
		ready:          make(chan struct{}),
		closed:         make(chan struct{}),
	}, nil
}

// Start connects to the server, performs the control handshake, and serves the
// control stream. It reconnects with exponential backoff until ctx is done or
// Close is called. Start blocks.
func (c *Client) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !c.started.CompareAndSwap(false, true) {
		return fmt.Errorf("client already started")
	}

	backoff := initialBackoff
	for {
		if c.isClosed() || ctx.Err() != nil {
			return nil
		}

		connected, err := c.runOnce(ctx)
		if c.isClosed() || ctx.Err() != nil {
			return nil
		}
		if connected {
			backoff = initialBackoff
		}
		if err != nil {
			c.log.Warn("control connection lost", "error", err)
		}

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-c.closed:
			timer.Stop()
			return nil
		case <-timer.C:
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// Close stops the control stream and the underlying HY2 client. It is
// idempotent.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)

		c.mu.Lock()
		conn := c.controlConn
		hy := c.hy
		c.controlConn = nil
		c.hy = nil
		c.connected = false
		dataConns := make([]net.Conn, 0, len(c.dataConns))
		for dataConn := range c.dataConns {
			dataConns = append(dataConns, dataConn)
		}
		c.dataConns = make(map[net.Conn]net.Conn)
		c.mu.Unlock()

		for _, dataConn := range dataConns {
			_ = dataConn.Close()
		}
		if conn != nil {
			_ = conn.Close()
		}
		if hy != nil {
			_ = hy.Close()
		}
		// The HY2 client is closed first so any blocked UDP send is released
		// before the mapping waits for its goroutines.
		c.stopUDPMappings()
	})
	return nil
}

// WaitReady blocks until the first successful control handshake, the client is
// closed, or ctx is done.
func (c *Client) WaitReady(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-c.ready:
		return nil
	default:
	}

	select {
	case <-c.ready:
		return nil
	case <-c.closed:
		return fmt.Errorf("client closed before the control handshake completed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Mappings returns a snapshot of the mapping read models.
func (c *Client) Mappings() []view.MappingView {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]view.MappingView(nil), c.mappings...)
}

func (c *Client) markReady() {
	c.readyOnce.Do(func() { close(c.ready) })
}

// markConnected records a completed control handshake.
func (c *Client) markConnected() {
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()
}

func (c *Client) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// setCurrent installs the live HY2 client and control stream unless the client
// has already been closed.
func (c *Client) setCurrent(hy hyclient.Client, conn net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.isClosed() {
		return false
	}
	c.hy = hy
	c.controlConn = conn
	return true
}

// clearCurrent removes the live connection only when it is still the one
// passed in, so a reconnect cannot clear a newer connection.
func (c *Client) clearCurrent(hy hyclient.Client, conn net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hy == hy {
		c.hy = nil
	}
	if c.controlConn == conn {
		c.controlConn = nil
	}
	c.connected = false
}

// setMappingResults stores per-mapping status from a HelloAck or UpdateAck.
func (c *Client) setMappingResults(mappings []config.Mapping, results []control.Result) {
	byID := make(map[string]control.Result, len(results))
	for _, result := range results {
		byID[result.ID] = result
	}

	views := mappingViews(mappings, byID)
	tokens := make(map[string]string)
	for _, result := range results {
		if result.UDPToken != "" {
			tokens[result.ID] = result.UDPToken
		}
	}
	configs := mappingConfigMap(mappings)

	c.mu.Lock()
	c.mappings = views
	c.mappingConfigs = configs
	c.udpTokens = tokens
	c.mu.Unlock()
}

// mappingConfigMap indexes mapping definitions by ID for data-plane lookups.
func mappingConfigMap(mappings []config.Mapping) map[string]config.Mapping {
	out := make(map[string]config.Mapping, len(mappings))
	for _, mapping := range mappings {
		out[mapping.ID] = mapping
	}
	return out
}

// mappingViews merges mapping definitions with server results. A nil results
// map produces the initial "pending" state used before the handshake.
func mappingViews(mappings []config.Mapping, results map[string]control.Result) []view.MappingView {
	views := make([]view.MappingView, 0, len(mappings))
	for _, mapping := range mappings {
		mappingView := view.MappingView{Mapping: mapping, Status: "pending"}
		if !mapping.Enabled {
			mappingView.Status = "disabled"
			views = append(views, mappingView)
			continue
		}
		if results == nil {
			views = append(views, mappingView)
			continue
		}
		result, ok := results[mapping.ID]
		switch {
		case !ok:
			mappingView.Status = "failed"
			mappingView.Error = "internal"
		case result.OK:
			mappingView.Status = "running"
			mappingView.AssignedPort = result.AssignedPort
		default:
			mappingView.Status = "failed"
			mappingView.Error = result.Error
		}
		views = append(views, mappingView)
	}
	return views
}

func newClientID() (string, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate client ID: %w", err)
	}
	return "c-" + hex.EncodeToString(id[:]), nil
}

func buildTLSConfig(cfg *config.ClientConfig) (hyclient.TLSConfig, error) {
	tlsConfig := hyclient.TLSConfig{
		ServerName:         cfg.TLS.SNI,
		InsecureSkipVerify: cfg.TLS.Insecure,
	}
	if tlsConfig.ServerName == "" {
		if host, _, err := net.SplitHostPort(cfg.Server); err == nil {
			tlsConfig.ServerName = host
		}
	}
	if cfg.TLS.CA != "" {
		pemData, err := os.ReadFile(cfg.TLS.CA)
		if err != nil {
			return hyclient.TLSConfig{}, fmt.Errorf("read TLS CA %s: %w", cfg.TLS.CA, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemData) {
			return hyclient.TLSConfig{}, fmt.Errorf("parse TLS CA %s: no certificates found", cfg.TLS.CA)
		}
		tlsConfig.RootCAs = pool
	}
	if cfg.TLS.PinSHA256 != "" {
		pin, err := parsePin(cfg.TLS.PinSHA256)
		if err != nil {
			return hyclient.TLSConfig{}, err
		}
		tlsConfig.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("server presented no certificate")
			}
			sum := sha256.Sum256(rawCerts[0])
			if subtle.ConstantTimeCompare(sum[:], pin) != 1 {
				return fmt.Errorf("server certificate pin mismatch")
			}
			return nil
		}
	}
	return tlsConfig, nil
}

func parsePin(value string) ([]byte, error) {
	text := strings.TrimSpace(value)
	text = strings.TrimPrefix(text, "sha256:")
	text = strings.ReplaceAll(text, ":", "")
	pin, err := hex.DecodeString(text)
	if err != nil || len(pin) != sha256.Size {
		return nil, fmt.Errorf("tls.pin_sha256 must be a 32-byte SHA-256 hex digest")
	}
	return pin, nil
}

func buildConnFactory(cfg *config.ClientConfig) (hyclient.ConnFactory, error) {
	switch cfg.Obfs.Type {
	case "":
		return nil, nil
	case "salamander":
		psk := []byte(cfg.Obfs.Salamander.Password)
		if len(psk) < 4 {
			return nil, fmt.Errorf("obfs.salamander.password must be at least 4 bytes")
		}
		return &salamanderConnFactory{psk: psk}, nil
	default:
		return nil, fmt.Errorf("unsupported obfs.type %q", cfg.Obfs.Type)
	}
}

// salamanderConnFactory wraps each new client UDP socket with Salamander
// obfuscation, matching the server-side packet connection.
type salamanderConnFactory struct {
	psk []byte
}

func (f *salamanderConnFactory) New(net.Addr) (net.PacketConn, error) {
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}
	wrapped, err := obfs.WrapPacketConnSalamander(conn, f.psk)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return wrapped, nil
}
