package client

import (
	"fmt"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/view"
)

// ConfigPath returns the path of the client configuration file. It is empty
// when the client was created from an in-memory configuration.
func (c *Client) ConfigPath() string {
	return c.configPath
}

// Revision returns the authoritative configuration revision.
func (c *Client) Revision() int64 {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	return c.cfg.Client.Revision
}

// AddMapping appends one mapping to the client configuration, bumps the
// revision, and writes the file atomically. If the control stream is online
// the change is announced with an update message; otherwise the next hello
// carries it.
func (c *Client) AddMapping(mapping config.Mapping) error {
	if err := c.editConfig(func(cfg *config.ClientConfig) error {
		if mapping.ID != "" {
			for _, existing := range cfg.Mappings {
				if existing.ID == mapping.ID {
					return fmt.Errorf("mapping %q: %w", mapping.ID, config.ErrMappingExists)
				}
			}
		}
		cfg.Mappings = append(cfg.Mappings, mapping)
		return nil
	}); err != nil {
		return err
	}
	c.afterLocalEdit()
	return nil
}

// UpdateMapping replaces one mapping by ID, bumps the revision, and writes the
// file atomically.
func (c *Client) UpdateMapping(mapping config.Mapping) error {
	if mapping.ID == "" {
		return fmt.Errorf("mapping ID is required")
	}
	if err := c.editConfig(func(cfg *config.ClientConfig) error {
		for i := range cfg.Mappings {
			if cfg.Mappings[i].ID == mapping.ID {
				cfg.Mappings[i] = mapping
				return nil
			}
		}
		return fmt.Errorf("mapping %q: %w", mapping.ID, config.ErrMappingNotFound)
	}); err != nil {
		return err
	}
	c.afterLocalEdit()
	return nil
}

// DeleteMapping removes one mapping by ID, bumps the revision, and writes the
// file atomically.
func (c *Client) DeleteMapping(id string) error {
	if id == "" {
		return fmt.Errorf("mapping ID is required")
	}
	if err := c.editConfig(func(cfg *config.ClientConfig) error {
		for i := range cfg.Mappings {
			if cfg.Mappings[i].ID != id {
				continue
			}
			cfg.Mappings = append(cfg.Mappings[:i], cfg.Mappings[i+1:]...)
			return nil
		}
		return fmt.Errorf("mapping %q: %w", id, config.ErrMappingNotFound)
	}); err != nil {
		return err
	}
	c.afterLocalEdit()
	return nil
}

// editConfig applies one local mutation to a copy of the configuration, bumps
// the revision, validates, and writes it. The in-memory configuration is only
// replaced after the write succeeds, so a failed write leaves both memory and
// revision untouched.
func (c *Client) editConfig(apply func(*config.ClientConfig) error) error {
	c.configMu.Lock()
	defer c.configMu.Unlock()

	if c.configPath == "" {
		return fmt.Errorf("client config path is not set")
	}
	next := cloneClientConfig(c.cfg)
	if err := apply(next); err != nil {
		return err
	}
	next.Client.Revision = c.cfg.Client.Revision + 1
	if err := next.Validate(); err != nil {
		return err
	}
	if err := writeConfigAtomic(c.configPath, next); err != nil {
		return err
	}

	tokens := retainTokensForEdit(c.cfg.Mappings, next.Mappings, c.udpTokenSnapshot())
	views := mergeViews(c.mappingSnapshot(), next.Mappings)
	c.storeConfigLocked(next, tokens, views)
	return nil
}

// afterLocalEdit reconciles local data-plane state and announces the new
// revision when the control stream is live.
func (c *Client) afterLocalEdit() {
	c.reconcileUDPMappings()
	c.sendUpdate()
}

// sendUpdate announces the current revision and complete mapping list. It is
// gated on a completed handshake: the control connection is installed before
// hello_ack, but an Update sent before Hello would be a protocol violation. A
// write failure is left to the reconnect loop; the local file remains
// authoritative.
func (c *Client) sendUpdate() {
	c.configMu.Lock()
	revision := c.cfg.Client.Revision
	// Copy into a non-nil slice even when empty: an explicit empty list clears
	// all server mappings, while a nil list means "no mapping change".
	mappings := make([]config.Mapping, len(c.cfg.Mappings))
	copy(mappings, c.cfg.Mappings)
	c.configMu.Unlock()

	c.mu.Lock()
	conn := c.controlConn
	connected := c.connected
	c.mu.Unlock()
	if !connected || conn == nil {
		return
	}
	if err := c.writeControl(conn, &control.Update{
		Type:     control.TypeUpdate,
		Revision: revision,
		Mappings: mappings,
	}); err != nil {
		c.log.Warn("send control update", "error", err)
	}
}

// writeConfigAtomic delegates to the config package's atomic writer.
func writeConfigAtomic(path string, cfg *config.ClientConfig) error {
	if path == "" {
		return fmt.Errorf("client config path is not set")
	}
	return config.SaveClient(path, cfg)
}

// cloneClientConfig copies cfg deeply enough for configuration edits. The
// mapping slice is the only mutable slice in ClientConfig.
func cloneClientConfig(cfg *config.ClientConfig) *config.ClientConfig {
	if cfg == nil {
		return nil
	}
	next := *cfg
	next.Mappings = append([]config.Mapping(nil), cfg.Mappings...)
	return &next
}

// snapshotConfig returns a copy of the current configuration.
func (c *Client) snapshotConfig() *config.ClientConfig {
	c.configMu.Lock()
	defer c.configMu.Unlock()
	return cloneClientConfig(c.cfg)
}

// storeConfigLocked installs a new configuration and its derived read models.
// The caller must hold configMu.
func (c *Client) storeConfigLocked(next *config.ClientConfig, tokens map[string]string, views []view.MappingView) {
	c.cfg = next

	c.mu.Lock()
	c.mappingConfigs = mappingConfigMap(next.Mappings)
	c.udpTokens = tokens
	c.mappings = views
	remaining := make(map[string]struct{}, len(next.Mappings))
	for _, mapping := range next.Mappings {
		remaining[mapping.ID] = struct{}{}
	}
	for id := range c.traffic {
		if _, ok := remaining[id]; !ok {
			delete(c.traffic, id)
		}
	}
	c.mu.Unlock()
}

func (c *Client) udpTokenSnapshot() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()

	tokens := make(map[string]string, len(c.udpTokens))
	for id, token := range c.udpTokens {
		tokens[id] = token
	}
	return tokens
}

func (c *Client) mappingSnapshot() []view.MappingView {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]view.MappingView(nil), c.mappings...)
}

// mergeViews preserves the status of mappings that survive a local edit
// unchanged. New or retargeted mappings become pending until the server
// acknowledges them.
func mergeViews(old []view.MappingView, mappings []config.Mapping) []view.MappingView {
	oldByID := make(map[string]view.MappingView, len(old))
	for _, mappingView := range old {
		oldByID[mappingView.ID] = mappingView
	}

	views := make([]view.MappingView, 0, len(mappings))
	for _, mapping := range mappings {
		mappingView := view.MappingView{Mapping: mapping, Status: "pending"}
		previous, ok := oldByID[mapping.ID]
		switch {
		case !mapping.Enabled:
			mappingView.Status = "disabled"
		case ok &&
			previous.Enabled == mapping.Enabled &&
			previous.Type == mapping.Type &&
			(mapping.RemotePort == 0 || mapping.RemotePort == previous.AssignedPort):
			mappingView.Status = previous.Status
			mappingView.Error = previous.Error
			mappingView.AssignedPort = previous.AssignedPort
		}
		views = append(views, mappingView)
	}
	return views
}

// retainTokensForEdit keeps a UDP token while its mapping still exists with
// the same protocol. A port change is reconciled after the update ack installs
// the replacement token.
func retainTokensForEdit(old, next []config.Mapping, tokens map[string]string) map[string]string {
	oldByID := mappingConfigMap(old)
	out := make(map[string]string)
	for _, mapping := range next {
		if !mapping.Enabled || mapping.Type != "udp" {
			continue
		}
		previous, ok := oldByID[mapping.ID]
		if !ok || previous.Type != mapping.Type {
			continue
		}
		if token := tokens[mapping.ID]; token != "" {
			out[mapping.ID] = token
		}
	}
	return out
}

// retainTokensForPush keeps a UDP token only when the pushed mapping keeps the
// same protocol and effective public port. A push cannot carry a replacement
// token, so changed UDP mappings wait for the next update ack or hello.
func retainTokensForPush(old, next []config.Mapping, tokens map[string]string, oldViews []view.MappingView) map[string]string {
	oldByID := mappingConfigMap(old)
	assigned := make(map[string]int, len(oldViews))
	for _, mappingView := range oldViews {
		assigned[mappingView.ID] = mappingView.AssignedPort
	}

	out := make(map[string]string)
	for _, mapping := range next {
		if !mapping.Enabled || mapping.Type != "udp" {
			continue
		}
		previous, ok := oldByID[mapping.ID]
		if !ok || previous.Type != mapping.Type {
			continue
		}
		oldPort := previous.RemotePort
		if oldPort == 0 {
			oldPort = assigned[mapping.ID]
		}
		if oldPort != 0 && mapping.RemotePort != 0 && oldPort != mapping.RemotePort {
			continue
		}
		if token := tokens[mapping.ID]; token != "" {
			out[mapping.ID] = token
		}
	}
	return out
}

// mergeHelloTokens keeps the UDP tokens from a HelloAck only for mappings that
// still match the snapshot sent in Hello. A mapping added or retargeted by a
// concurrent local edit waits for the update ack instead.
func mergeHelloTokens(snapshot, current []config.Mapping, results []control.Result) map[string]string {
	byID := resultMap(results)
	snapshotByID := mappingConfigMap(snapshot)
	tokens := make(map[string]string)
	for _, mapping := range current {
		if !mapping.Enabled || mapping.Type != "udp" {
			continue
		}
		previous, ok := snapshotByID[mapping.ID]
		if !ok || previous.Type != mapping.Type || previous.Enabled != mapping.Enabled {
			continue
		}
		result, ok := byID[mapping.ID]
		if !ok || !result.OK || result.UDPToken == "" {
			continue
		}
		oldPort := previous.RemotePort
		if oldPort == 0 {
			oldPort = result.AssignedPort
		}
		if oldPort != 0 && mapping.RemotePort != 0 && oldPort != mapping.RemotePort {
			continue
		}
		tokens[mapping.ID] = result.UDPToken
	}
	return tokens
}

// helloViews builds read models for the current config using the HelloAck
// results for mappings that still match the hello snapshot. Concurrent local
// edits stay pending instead of being overwritten or marked failed.
func helloViews(snapshot, current []config.Mapping, results []control.Result) []view.MappingView {
	byID := resultMap(results)
	snapshotByID := mappingConfigMap(snapshot)
	views := make([]view.MappingView, 0, len(current))
	for _, mapping := range current {
		mappingView := view.MappingView{Mapping: mapping, Status: "pending"}
		switch {
		case !mapping.Enabled:
			mappingView.Status = "disabled"
		default:
			previous, ok := snapshotByID[mapping.ID]
			if !ok || previous.Type != mapping.Type || previous.Enabled != mapping.Enabled {
				break
			}
			result, ok := byID[mapping.ID]
			if !ok {
				break
			}
			oldPort := previous.RemotePort
			if oldPort == 0 {
				oldPort = result.AssignedPort
			}
			if oldPort != 0 && mapping.RemotePort != 0 && oldPort != mapping.RemotePort {
				break
			}
			if result.OK {
				mappingView.Status = "running"
				mappingView.AssignedPort = result.AssignedPort
			} else {
				mappingView.Status = "failed"
				mappingView.Error = result.Error
			}
		}
		views = append(views, mappingView)
	}
	return views
}

// applyPushTokens overlays the server's per-mapping push results on the
// retained token map. A result with no token removes any stale token for that
// mapping, for example after a protocol or port change.
func applyPushTokens(tokens map[string]string, mappings []config.Mapping, results []control.Result) {
	if len(results) == 0 {
		return
	}
	byID := resultMap(results)
	for _, mapping := range mappings {
		result, ok := byID[mapping.ID]
		if !ok {
			continue
		}
		if result.OK && result.UDPToken != "" && mapping.Enabled && mapping.Type == "udp" {
			tokens[mapping.ID] = result.UDPToken
			continue
		}
		delete(tokens, mapping.ID)
	}
}

// pushViews builds the read models for a pushed mapping list. A UDP mapping
// without a token stays pending because push does not carry tokens.
func pushViews(mappings []config.Mapping, tokens map[string]string) []view.MappingView {
	views := make([]view.MappingView, 0, len(mappings))
	for _, mapping := range mappings {
		mappingView := view.MappingView{Mapping: mapping}
		switch {
		case !mapping.Enabled:
			mappingView.Status = "disabled"
		case mapping.Type == "udp" && tokens[mapping.ID] == "":
			mappingView.Status = "pending"
		default:
			mappingView.Status = "running"
			mappingView.AssignedPort = mapping.RemotePort
		}
		views = append(views, mappingView)
	}
	return views
}

// pushResults reports the applied push back to the server.
func pushResults(mappings []config.Mapping, tokens map[string]string) []control.Result {
	results := make([]control.Result, 0, len(mappings))
	for _, mapping := range mappings {
		result := control.Result{ID: mapping.ID, OK: true}
		if mapping.Enabled {
			result.AssignedPort = mapping.RemotePort
			result.UDPToken = tokens[mapping.ID]
		}
		results = append(results, result)
	}
	return results
}

// applyAssignedPorts writes server-assigned ports into mappings that requested
// port 0. It returns a copy and whether anything changed.
func applyAssignedPorts(mappings []config.Mapping, results []control.Result) ([]config.Mapping, bool) {
	byID := make(map[string]control.Result, len(results))
	for _, result := range results {
		byID[result.ID] = result
	}

	out := append([]config.Mapping(nil), mappings...)
	changed := false
	for i := range out {
		if out[i].RemotePort != 0 {
			continue
		}
		result, ok := byID[out[i].ID]
		if !ok || !result.OK || result.AssignedPort <= 0 {
			continue
		}
		out[i].RemotePort = result.AssignedPort
		changed = true
	}
	return out, changed
}

// validatePushedMappings rejects a push whose identity or shape could not be
// represented in the local configuration.
func validatePushedMappings(mappings []config.Mapping) error {
	seen := make(map[string]struct{}, len(mappings))
	for i, mapping := range mappings {
		if mapping.ID == "" {
			return fmt.Errorf("pushed mapping %d has no ID", i)
		}
		if _, duplicate := seen[mapping.ID]; duplicate {
			return fmt.Errorf("pushed mapping %q is duplicated", mapping.ID)
		}
		seen[mapping.ID] = struct{}{}
		if mapping.Type != "tcp" && mapping.Type != "udp" {
			return fmt.Errorf("pushed mapping %q has invalid type %q", mapping.ID, mapping.Type)
		}
		if mapping.RemotePort < 0 || mapping.RemotePort > 65535 {
			return fmt.Errorf("pushed mapping %q has an invalid remote port", mapping.ID)
		}
	}
	return nil
}
