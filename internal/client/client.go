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
	"github.com/apernet/hysteria/extras/v2/transport/udphop"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/proxy"
	"hy2tun/internal/version"
	"hy2tun/internal/view"
	"hy2tun/internal/webui/clientui"
)

const (
	controlAddress = "_hy2tun_control_:0"

	initialBackoff = time.Second
	maxBackoff     = 30 * time.Second
)

// mappingTraffic holds the byte counters for one mapping. Tx is client-to-server
// (up) and Rx is server-to-client (down).
type mappingTraffic struct {
	tx atomic.Uint64
	rx atomic.Uint64
}

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

	mu          sync.Mutex
	hy          hyclient.Client
	controlConn net.Conn
	connected   bool
	// credentialsGeneration mirrors cfg.Credentials.Generation. The server
	// fields are the last HelloAck values; 0/false mean the server predates
	// credential rotation, so the generation is unknown rather than 1.
	credentialsGeneration      int64
	serverCredentialGeneration int64
	credentialSynced           bool
	mappings                   []view.MappingView
	mappingConfigs             map[string]config.Mapping
	udpTokens                  map[string]string
	udpMappings                map[string]*udpMapping
	traffic                    map[string]*mappingTraffic
	// dataConns maps a live data-plane connection to the control stream that
	// owns it, so a dropped stream cannot leak local dials.
	dataConns map[net.Conn]net.Conn
	writeMu   sync.Mutex

	// logs is the bounded ring that backs the client web console. web is nil
	// when web.listen is empty.
	logs *logBuffer
	web  *clientui.Server

	// proxies are the optional local SOCKS5/HTTP CONNECT listeners. They are
	// bound during New and their accept loops run with the client.
	proxies []*proxy.Server

	// configMu serializes local configuration edits, server pushes, and the
	// assigned-port write-backs that follow a handshake. The client config is
	// the single source of truth, so every revision decision is made under it.
	configMu   sync.Mutex
	configPath string
	// udpReconcileMu serializes UDP session reconciliation so a push and an
	// update ack cannot start duplicate sessions for one mapping.
	udpReconcileMu sync.Mutex

	ready     chan struct{}
	readyOnce sync.Once
	closed    chan struct{}
	closeOnce sync.Once
	started   atomic.Bool
}

var (
	_ clientui.API = (*Client)(nil)
)

// New validates cfg and prepares the HY2 client configuration. It does not
// connect; call Start for that.
func New(cfg *config.ClientConfig, log *slog.Logger) (*Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("client config is nil")
	}
	if log == nil {
		log = slog.Default()
	}
	logs := newLogBuffer(logBufferSize)
	log = slog.New(teeHandler{
		primary:   log.Handler(),
		secondary: slog.NewJSONHandler(logs, nil),
	})
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

	serverAddr, err := resolveServerAddr(cfg)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	connFactory, err := buildConnFactory(cfg, serverAddr)
	if err != nil {
		return nil, err
	}
	credentialsGeneration := cfg.Credentials.Generation
	if credentialsGeneration < 1 {
		credentialsGeneration = 1
	}

	cl := &Client{
		cfg:                   cfg,
		log:                   log,
		serverAddr:            serverAddr,
		tlsConfig:             tlsConfig,
		connFactory:           connFactory,
		newHY2:                hyclient.NewClient,
		startedAt:             time.Now(),
		configPath:            cfg.ConfigPath,
		credentialsGeneration: credentialsGeneration,
		mappings:              mappingViews(cfg.Mappings, nil),
		mappingConfigs:        mappingConfigMap(cfg.Mappings),
		udpTokens:             make(map[string]string),
		udpMappings:           make(map[string]*udpMapping),
		traffic:               make(map[string]*mappingTraffic),
		dataConns:             make(map[net.Conn]net.Conn),
		logs:                  logs,
		ready:                 make(chan struct{}),
		closed:                make(chan struct{}),
	}
	if cfg.WebEnabled() {
		sessionKey, err := loadClientWebSessionKey(cfg, log)
		if err != nil {
			return nil, err
		}
		web, err := clientui.New(cl, cfg.Web, sessionKey, log)
		if err != nil {
			return nil, fmt.Errorf("create client web console: %w", err)
		}
		cl.web = web
	}
	if err := cl.initProxies(); err != nil {
		if cl.web != nil {
			_ = cl.web.Close()
		}
		return nil, err
	}
	if cfg.ServerPortHopping() {
		minHop, maxHop, _ := cfg.HopInterval()
		log.Warn("port hopping enabled; server must redirect the entire UDP range to the HY2 listen port",
			"server", cfg.Server, "hop_min_interval", minHop.String(), "hop_max_interval", maxHop.String())
	}
	return cl, nil
}

// initProxies binds the optional local proxy listeners. Binding happens during
// New so a bad listen address or credentials fail startup before the client
// attempts to connect. Start runs the accept loops.
func (c *Client) initProxies() error {
	if !c.cfg.ProxyEnabled() {
		return nil
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" {
			return nil, fmt.Errorf("proxy supports TCP only, got %q", network)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return c.TCP(addr)
	}

	if listen := c.cfg.Proxy.SOCKS5; listen != "" {
		srv, err := proxy.NewSOCKS5(listen, dial, c.log, c.cfg.Proxy.SOCKS5Auth)
		if err != nil {
			c.closeProxies()
			return fmt.Errorf("create SOCKS5 proxy on %s: %w", listen, err)
		}
		c.proxies = append(c.proxies, srv)
	}
	if listen := c.cfg.Proxy.HTTP; listen != "" {
		srv, err := proxy.NewHTTP(listen, dial, c.log, c.cfg.Proxy.HTTPAuth)
		if err != nil {
			c.closeProxies()
			return fmt.Errorf("create HTTP proxy on %s: %w", listen, err)
		}
		c.proxies = append(c.proxies, srv)
	}
	return nil
}

// startProxies runs each bound proxy's accept loop. The listeners are already
// bound, so Start only fails on an unexpected accept error.
func (c *Client) startProxies() {
	for _, srv := range c.proxies {
		c.log.Info("local proxy listening", "addr", srv.Addr())
		go func(srv *proxy.Server) {
			if err := srv.Start(); err != nil {
				c.log.Warn("local proxy stopped", "addr", srv.Addr(), "error", err)
			}
		}(srv)
	}
}

// closeProxies closes every proxy listener and its active connections. Server
// Close is idempotent, so this is safe from both New's error path and Client
// shutdown.
func (c *Client) closeProxies() {
	for _, srv := range c.proxies {
		if err := srv.Close(); err != nil {
			c.log.Warn("close local proxy", "addr", srv.Addr(), "error", err)
		}
	}
}

// loadClientWebSessionKey loads the persisted web session signing key next to
// the client configuration. A configuration without a path gets an ephemeral
// key, which is safe but invalidates sessions on restart.
func loadClientWebSessionKey(cfg *config.ClientConfig, log *slog.Logger) ([]byte, error) {
	if cfg.ConfigPath == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate web session key: %w", err)
		}
		log.Warn("client web session key is ephemeral because the config path is unknown")
		return key, nil
	}
	key, err := config.LoadOrCreateWebSecret(config.WebSecretPath(cfg.ConfigPath))
	if err != nil {
		return nil, fmt.Errorf("load client web session key: %w", err)
	}
	return key, nil
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
	if c.web != nil {
		if err := c.web.Start(); err != nil {
			return fmt.Errorf("start client web console: %w", err)
		}
	}
	c.startProxies()

	backoff := initialBackoff
	for {
		if c.isClosed() || ctx.Err() != nil {
			return nil
		}

		attemptedAt := time.Now()
		connected, err := c.runOnce(ctx)
		if c.isClosed() || ctx.Err() != nil {
			return nil
		}
		// Reset the backoff only after a connection that stayed healthy for a
		// full cap interval. A server that accepts and immediately drops the
		// stream must not be retried every second forever.
		if connected && time.Since(attemptedAt) >= maxBackoff {
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
		if c.web != nil {
			if err := c.web.Close(); err != nil {
				c.log.Warn("close client web console", "error", err)
			}
		}

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
		// The HY2 client is closed before waiting for proxy handlers, so a
		// stream request blocked in hy.TCP is released. Proxy listeners stop
		// accepting and their active connections are closed here.
		c.closeProxies()
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

// Mappings returns a snapshot of the mapping read models with the latest
// per-mapping traffic counters.
func (c *Client) Mappings() []view.MappingView {
	c.mu.Lock()
	defer c.mu.Unlock()
	views := append([]view.MappingView(nil), c.mappings...)
	for i := range views {
		if traffic := c.traffic[views[i].ID]; traffic != nil {
			views[i].TxBytes = traffic.tx.Load()
			views[i].RxBytes = traffic.rx.Load()
		}
	}
	return views
}

// Status returns the local connection status and aggregate traffic.
func (c *Client) Status() view.ClientStatus {
	c.configMu.Lock()
	name := c.cfg.Client.Name
	server := c.cfg.Server
	c.configMu.Unlock()

	c.mu.Lock()
	connected := c.connected
	credentialGeneration := c.credentialsGeneration
	serverCredentialGeneration := c.serverCredentialGeneration
	credentialSynced := c.credentialSynced
	c.mu.Unlock()

	mappings := c.Mappings()
	var txBytes, rxBytes uint64
	for _, mapping := range mappings {
		txBytes += mapping.TxBytes
		rxBytes += mapping.RxBytes
	}
	return view.ClientStatus{
		Name:                       name,
		Server:                     server,
		Version:                    version.Version,
		Connected:                  connected,
		Uptime:                     time.Since(c.startedAt),
		TxBytes:                    txBytes,
		RxBytes:                    rxBytes,
		CredentialGeneration:       credentialGeneration,
		ServerCredentialGeneration: serverCredentialGeneration,
		CredentialSynced:           credentialSynced,
	}
}

// Logs returns recent client log entries at or above level.
func (c *Client) Logs(level string, limit int) []view.LogEntry {
	if c.logs == nil {
		return nil
	}
	return c.logs.Entries(level, limit)
}

// Settings returns the secret-free settings snapshot for the web console. The
// HY2 password and control token are masked here; their plaintext is only
// available through the authenticated reveal endpoint.
func (c *Client) Settings() clientui.SettingsView {
	c.configMu.Lock()
	cfg := c.cfg
	configPath := c.configPath
	c.configMu.Unlock()

	c.mu.Lock()
	serverGeneration := c.serverCredentialGeneration
	synced := c.credentialSynced
	c.mu.Unlock()

	generation := cfg.Credentials.Generation
	if generation < 1 {
		generation = 1
	}

	tls := "已启用"
	if cfg.TLS.Insecure {
		tls += "（跳过证书校验）"
	}
	if cfg.TLS.SNI != "" {
		tls += " · SNI " + cfg.TLS.SNI
	}
	obfs := cfg.Obfs.Type
	if obfs == "" {
		obfs = "未启用"
	}
	webListen := cfg.Web.Listen
	if webListen == "" {
		webListen = "未启用"
	}
	configFile := configPath
	if configFile == "" {
		configFile = "—"
	}

	return clientui.SettingsView{
		WebUsername:           cfg.Web.Username,
		WebPasswordConfigured: cfg.Web.PasswordHash != "",
		Name:                  cfg.Client.Name,
		Credentials: clientui.CredentialSettingsView{
			Generation:                generation,
			ServerGeneration:          serverGeneration,
			CurrentAuthMasked:         maskClientSecret(cfg.Auth),
			CurrentControlTokenMasked: maskClientSecret(cfg.Control.Token),
			Synced:                    synced,
		},
		ReadOnly: []clientui.ReadOnlySetting{
			{Label: "服务器地址", Value: cfg.Server},
			{Label: "TLS", Value: tls},
			{Label: "Obfs", Value: obfs},
			{Label: "带宽上行", Value: formatClientBandwidth(cfg.Bandwidth.Up)},
			{Label: "带宽下行", Value: formatClientBandwidth(cfg.Bandwidth.Down)},
			{Label: "忽略客户端带宽", Value: yesNo(cfg.Bandwidth.IgnoreClient)},
			{Label: "SOCKS5 代理", Value: proxyReadOnlyValue(cfg.Proxy.SOCKS5, cfg.Proxy.SOCKS5Auth)},
			{Label: "HTTP 代理", Value: proxyReadOnlyValue(cfg.Proxy.HTTP, cfg.Proxy.HTTPAuth)},
			{Label: "客户端 ID", Value: cfg.Client.ID},
			{Label: "Web 监听", Value: webListen},
			{Label: "配置文件", Value: configFile},
		},
	}
}

// formatClientBandwidth renders a configured HY2 bandwidth limit. Zero means
// unlimited.
func formatClientBandwidth(value config.Bandwidth) string {
	bytes := uint64(value)
	if bytes == 0 {
		return "未限制"
	}
	switch {
	case bytes%(1<<30) == 0:
		return fmt.Sprintf("%d GiB/s", bytes>>30)
	case bytes%(1<<20) == 0:
		return fmt.Sprintf("%d MiB/s", bytes>>20)
	case bytes%(1<<10) == 0:
		return fmt.Sprintf("%d KiB/s", bytes>>10)
	default:
		return fmt.Sprintf("%d B/s", bytes)
	}
}

// proxyReadOnlyValue reports a local proxy listener without exposing its
// authentication secret.
func proxyReadOnlyValue(listen, auth string) string {
	if listen == "" {
		return "未启用"
	}
	if auth != "" {
		return listen + "（需要认证）"
	}
	return listen
}

func yesNo(value bool) string {
	if value {
		return "是"
	}
	return "否"
}

func maskClientSecret(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return clientui.MaskedSecret
}

// WebAddr returns the bound client web address, or the configured address when
// the web console is disabled or has not started yet.
func (c *Client) WebAddr() string {
	if c.web != nil {
		return c.web.Addr()
	}
	c.configMu.Lock()
	defer c.configMu.Unlock()
	return c.cfg.Web.Listen
}

// TCP opens a TCP stream to addr through the live HY2 connection. It is the
// dial path used by the local proxy listeners.
func (c *Client) TCP(addr string) (net.Conn, error) {
	if addr == "" {
		return nil, fmt.Errorf("address is required")
	}
	hy := c.hyClient()
	if hy == nil {
		return nil, fmt.Errorf("client is not connected")
	}
	conn, err := hy.TCP(addr)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, fmt.Errorf("HY2 returned a nil TCP stream")
	}
	return conn, nil
}

// trafficFor returns the shared counters for a mapping, creating them on first
// use. An empty mapping ID has no counters.
func (c *Client) trafficFor(id string) *mappingTraffic {
	if id == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.traffic == nil {
		c.traffic = make(map[string]*mappingTraffic)
	}
	traffic := c.traffic[id]
	if traffic == nil {
		traffic = &mappingTraffic{}
		c.traffic[id] = traffic
	}
	return traffic
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
	views := mappingViews(mappings, resultMap(results))
	tokens := tokensFromResults(results)
	configs := mappingConfigMap(mappings)

	c.mu.Lock()
	c.mappings = views
	c.mappingConfigs = configs
	c.udpTokens = tokens
	c.mu.Unlock()
}

// resultMap indexes server results by mapping ID.
func resultMap(results []control.Result) map[string]control.Result {
	byID := make(map[string]control.Result, len(results))
	for _, result := range results {
		byID[result.ID] = result
	}
	return byID
}

// tokensFromResults extracts UDP tokens from server results. Tokens are never
// logged or persisted.
func tokensFromResults(results []control.Result) map[string]string {
	tokens := make(map[string]string)
	for _, result := range results {
		if result.UDPToken != "" {
			tokens[result.ID] = result.UDPToken
		}
	}
	return tokens
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
		if host, _, err := net.SplitHostPort(strings.TrimSpace(cfg.Server)); err == nil {
			tlsConfig.ServerName = strings.TrimSpace(host)
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

func resolveServerAddr(cfg *config.ClientConfig) (net.Addr, error) {
	normalized, hopping, err := cfg.NormalizedServer()
	if err != nil {
		return nil, err
	}
	if hopping {
		hopServer, err := normalizeHopServer(normalized)
		if err != nil {
			return nil, fmt.Errorf("resolve server address %s: %w", normalized, err)
		}
		addr, err := udphop.ResolveUDPHopAddr(hopServer)
		if err != nil {
			return nil, fmt.Errorf("resolve server address %s: %w", normalized, err)
		}
		return addr, nil
	}
	addr, err := net.ResolveUDPAddr("udp", normalized)
	if err != nil {
		return nil, fmt.Errorf("resolve server address %s: %w", normalized, err)
	}
	return addr, nil
}

// normalizeHopServer canonicalizes a port-hopping server address. Config
// validation trims whitespace around port tokens, but udphop's port parser
// does not, so spaces accepted by validation must be removed before resolving.
func normalizeHopServer(value string) (string, error) {
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return "", fmt.Errorf("server must include a host")
	}
	portStr = strings.Join(strings.Fields(portStr), "")
	return net.JoinHostPort(host, portStr), nil
}

func buildConnFactory(cfg *config.ClientConfig, serverAddr net.Addr) (hyclient.ConnFactory, error) {
	var psk []byte
	switch cfg.Obfs.Type {
	case "":
	case "salamander":
		psk = []byte(cfg.Obfs.Salamander.Password)
		if len(psk) < 4 {
			return nil, fmt.Errorf("obfs.salamander.password must be at least 4 bytes")
		}
	default:
		return nil, fmt.Errorf("unsupported obfs.type %q", cfg.Obfs.Type)
	}

	if hopAddr, ok := serverAddr.(*udphop.UDPHopAddr); ok {
		minHop, maxHop, err := cfg.HopInterval()
		if err != nil {
			return nil, err
		}
		return &hopConnFactory{
			addr:     hopAddr,
			interval: udphop.HopIntervalConfig{Min: minHop, Max: maxHop},
			psk:      psk,
		}, nil
	}
	if len(psk) == 0 {
		return nil, nil
	}
	return &salamanderConnFactory{psk: psk}, nil
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

// hopConnFactory creates a new UDP hop packet connection for every HY2
// connection. The factory itself is reusable across client reconnects.
type hopConnFactory struct {
	addr      *udphop.UDPHopAddr
	interval  udphop.HopIntervalConfig
	psk       []byte
	listenUDP udphop.ListenUDPFunc
}

func (f *hopConnFactory) New(net.Addr) (net.PacketConn, error) {
	open := f.listenUDP
	if open == nil {
		open = func() (net.PacketConn, error) {
			return net.ListenUDP("udp", nil)
		}
	}
	if len(f.psk) > 0 {
		base := open
		open = func() (net.PacketConn, error) {
			conn, err := base()
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
	}
	return udphop.NewUDPHopPacketConn(f.addr, f.interval, open)
}
