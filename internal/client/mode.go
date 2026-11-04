package client

import (
	"fmt"

	"hy2tun/internal/config"
)

// UpdateMode validates and persists a new effective transport mode. The
// configuration is written before the running state changes, so a failed save
// leaves the current owner, credentials and probe policy untouched. An active
// owner whose kind remains allowed is retained; otherwise its session is
// cancelled and the existing retry loop reconnects with the new mode.
func (c *Client) UpdateMode(mode config.TransportMode) error {
	if c == nil {
		return fmt.Errorf("update mode: client is nil")
	}
	if err := mode.Validate(); err != nil {
		return fmt.Errorf("update mode: %w", err)
	}

	c.configMu.Lock()
	defer c.configMu.Unlock()

	if c.isClosed() {
		return fmt.Errorf("update mode: client is closed")
	}
	next := cloneClientConfig(c.cfg)
	if next == nil {
		return fmt.Errorf("update mode: client config is nil")
	}
	next.Mode = mode
	if err := next.Validate(); err != nil {
		return fmt.Errorf("update mode: %w", err)
	}
	if err := c.writeConfig(next); err != nil {
		return fmt.Errorf("update mode: %w", err)
	}
	c.storeConfigLocked(next, c.udpTokenSnapshot(), c.mappingSnapshot())
	return nil
}
