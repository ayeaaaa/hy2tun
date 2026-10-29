package client

import (
	"fmt"
	"net"
	"strconv"

	"hy2tun/internal/config"
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

	c.configMu.Lock()
	oldCfg := c.cfg.Proxy
	c.configMu.Unlock()

	hadProxies := len(c.proxies) > 0
	c.closeProxiesLocked()
	if !cfg.Enabled {
		c.configMu.Lock()
		c.cfg.Proxy = cfg
		c.configMu.Unlock()
		c.proxyErr = ""
		return nil
	}

	proxies, err := c.bindProxiesLocked(cfg)
	if err != nil {
		// Best effort: restore the previous listener set so a failed hot update
		// does not disable a working proxy.
		if hadProxies {
			restored, restoreErr := c.bindProxiesLocked(oldCfg)
			if restoreErr != nil {
				c.proxyErr = fmt.Sprintf("%v; restore failed: %v", err, restoreErr)
			} else {
				c.proxies = restored
				c.proxyErr = err.Error()
				if c.started.Load() {
					c.startProxiesLocked()
				}
			}
		} else {
			// A startup bind failure leaves this server's proxy disabled but
			// retains the requested addresses for the web console to display.
			c.configMu.Lock()
			c.cfg.Proxy = cfg
			c.cfg.Proxy.Enabled = false
			c.configMu.Unlock()
			c.proxyErr = err.Error()
		}
		return err
	}

	c.proxies = proxies
	c.configMu.Lock()
	c.cfg.Proxy = cfg
	c.configMu.Unlock()
	c.proxyErr = ""
	if c.started.Load() {
		c.startProxiesLocked()
	}
	return nil
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
