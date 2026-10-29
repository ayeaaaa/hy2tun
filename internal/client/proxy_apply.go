package client

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"hy2tun/internal/config"
	"hy2tun/internal/proxy"
	"hy2tun/internal/webui/clientui"
)

// ProxyState is the current per-server proxy state exposed to the web console.
type ProxyState struct {
	Enabled bool
	SOCKS5  string
	HTTP    string
	Error   string
}

// ApplyProxy replaces the worker's local proxy listeners. The previous
// listeners are closed before binding the new set; if binding fails, the old
// listeners are restored and the previous configuration is kept. This is a
// deliberate trade-off: a proxy address change briefly interrupts active proxy
// connections instead of risking two listeners on one address.
func (c *Client) ApplyProxy(cfg config.ProxyConfig) error {
	if c.isClosed() {
		return fmt.Errorf("client is closed")
	}
	if cfg.Enabled {
		if cfg.SOCKS5 == "" && cfg.HTTP == "" {
			return fmt.Errorf("proxy.enabled requires socks5 or http")
		}
		for _, value := range []struct {
			name string
			addr string
		}{{"socks5", cfg.SOCKS5}, {"http", cfg.HTTP}} {
			if value.addr == "" {
				continue
			}
			if err := validateProxyAddress(value.name, value.addr); err != nil {
				return err
			}
		}
	}

	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()

	oldByAddr := make(map[string]*proxy.Server, len(c.proxies))
	for _, srv := range c.proxies {
		oldByAddr[srv.Addr()] = srv
	}

	if !cfg.Enabled {
		c.closeProxiesLocked()
		c.proxies = nil
		c.configMu.Lock()
		c.cfg.Proxy = cfg
		c.configMu.Unlock()
		c.proxyErr = ""
		return nil
	}

	desired := []struct {
		kind string
		addr string
	}{{"socks5", cfg.SOCKS5}, {"http", cfg.HTTP}}
	newProxies := make([]*proxy.Server, 0, 2)
	added := make([]*proxy.Server, 0, 2)
	for _, item := range desired {
		if item.addr == "" {
			continue
		}
		if srv := oldByAddr[item.addr]; srv != nil {
			newProxies = append(newProxies, srv)
			continue
		}
		srv, err := c.bindOneProxy(item.kind, item.addr, cfg)
		if err != nil {
			for _, addedSrv := range added {
				_ = addedSrv.Close()
			}
			if len(c.proxies) == 0 {
				c.configMu.Lock()
				c.cfg.Proxy = cfg
				c.cfg.Proxy.Enabled = false
				c.configMu.Unlock()
			}
			c.proxyErr = err.Error()
			return err
		}
		newProxies = append(newProxies, srv)
		added = append(added, srv)
	}

	newSet := make(map[string]struct{}, len(newProxies))
	for _, srv := range newProxies {
		newSet[srv.Addr()] = struct{}{}
	}
	for _, old := range c.proxies {
		if _, keep := newSet[old.Addr()]; !keep {
			if err := old.Close(); err != nil {
				c.log.Warn("close removed local proxy", "addr", old.Addr(), "error", err)
			}
		}
	}
	c.proxies = newProxies
	c.configMu.Lock()
	c.cfg.Proxy = cfg
	c.configMu.Unlock()
	c.proxyErr = ""
	if c.started.Load() {
		c.startProxyListLocked(added)
	}
	return nil
}

func (c *Client) bindOneProxy(kind, listen string, cfg config.ProxyConfig) (*proxy.Server, error) {
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" {
			return nil, fmt.Errorf("proxy supports TCP only, got %q", network)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return c.TCP(addr)
	}
	switch kind {
	case "socks5":
		srv, err := proxy.NewSOCKS5(listen, dial, c.log, cfg.SOCKS5Auth)
		if err != nil {
			return nil, fmt.Errorf("create SOCKS5 proxy on %s: %w", listen, err)
		}
		return srv, nil
	case "http":
		srv, err := proxy.NewHTTP(listen, dial, c.log, cfg.HTTPAuth)
		if err != nil {
			return nil, fmt.Errorf("create HTTP proxy on %s: %w", listen, err)
		}
		return srv, nil
	default:
		return nil, fmt.Errorf("unknown proxy kind %q", kind)
	}
}

func (c *Client) startProxyListLocked(proxies []*proxy.Server) {
	for _, srv := range proxies {
		c.log.Info("local proxy listening", "addr", srv.Addr())
		go func(srv *proxy.Server) {
			if err := srv.Start(); err != nil {
				c.log.Warn("local proxy stopped", "addr", srv.Addr(), "error", err)
			}
		}(srv)
	}
}

// ProxyState returns a snapshot of the current proxy configuration and the
// last bind error.
func (c *Client) ProxyState() ProxyState {
	c.proxyMu.Lock()
	defer c.proxyMu.Unlock()
	c.configMu.Lock()
	cfg := c.cfg.Proxy
	c.configMu.Unlock()
	return ProxyState{
		Enabled: cfg.Enabled,
		SOCKS5:  cfg.SOCKS5,
		HTTP:    cfg.HTTP,
		Error:   c.proxyErr,
	}
}

func validateProxyAddress(name, value string) error {
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("proxy.%s must be a valid host:port: %w", name, err)
	}
	if host == "" {
		return fmt.Errorf("proxy.%s must include a host", name)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("proxy.%s port must be between 1 and 65535", name)
	}
	return nil
}

// Proxy returns the web-console proxy view for this worker.
func (c *Client) Proxy() clientui.ProxyView {
	state := c.ProxyState()
	return clientui.ProxyView{
		Enabled: state.Enabled,
		SOCKS5:  state.SOCKS5,
		HTTP:    state.HTTP,
		Error:   state.Error,
	}
}

// UpdateProxy applies and persists a proxy change for a standalone client. The
// supervisor uses its own transactional update path for multi-server configs.
func (c *Client) UpdateProxy(cfg config.ProxyConfig) error {
	c.configMu.Lock()
	old := c.cfg.Proxy
	c.configMu.Unlock()
	cfg = mergeProxyAuth(cfg, old)
	if err := c.ApplyProxy(cfg); err != nil {
		return err
	}
	c.configMu.Lock()
	next := cloneClientConfig(c.cfg)
	next.Proxy = cfg
	c.configMu.Unlock()
	if err := c.writeConfig(next); err != nil {
		if restoreErr := c.ApplyProxy(old); restoreErr != nil {
			c.log.Warn("restore proxy after config save failure", "error", restoreErr)
		}
		return err
	}
	return nil
}
