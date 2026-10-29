package config

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// ErrProxyConflict reports a proxy listener address conflict.
var ErrProxyConflict = errors.New("proxy address conflict")

// ClientServerConfig is one independent remote server in a multi-server client
// configuration. It mirrors the per-server fields of ClientConfig and never
// shares mappings, credentials, or proxy listeners with another server.
type ClientServerConfig struct {
	Name        string                 `yaml:"name" json:"name"`
	Server      string                 `yaml:"server" json:"server"`
	Auth        string                 `yaml:"auth" json:"-"`
	TLS         ClientTLSConfig        `yaml:"tls" json:"tls"`
	Obfs        ObfsConfig             `yaml:"obfs" json:"obfs"`
	Bandwidth   BandwidthConfig        `yaml:"bandwidth" json:"bandwidth"`
	Transport   *ClientTransportConfig `yaml:"transport,omitempty" json:"transport,omitempty"`
	Control     ControlConfig          `yaml:"control" json:"control"`
	Client      ClientIdentity         `yaml:"client" json:"client"`
	Credentials CredentialsConfig      `yaml:"credentials" json:"credentials"`
	Mappings    []Mapping              `yaml:"mappings" json:"mappings"`
	Proxy       ProxyConfig            `yaml:"proxy" json:"proxy"`
}

// IsMulti reports whether the config uses the independent servers list.
func (c *ClientConfig) IsMulti() bool {
	return c != nil && len(c.Servers) > 0
}

// ServerConfigs returns independent server snapshots. Legacy configs are
// represented as one server named "default" without mutating the YAML shape.
func (c *ClientConfig) ServerConfigs() []ClientServerConfig {
	if c == nil {
		return nil
	}
	if c.IsMulti() {
		out := make([]ClientServerConfig, len(c.Servers))
		for i, server := range c.Servers {
			out[i] = server.clone()
		}
		return out
	}
	proxy := c.Proxy
	if proxy.SOCKS5 != "" || proxy.HTTP != "" {
		proxy.Enabled = true
	}
	return []ClientServerConfig{{
		Name:        "default",
		Server:      c.Server,
		Auth:        c.Auth,
		TLS:         c.TLS,
		Obfs:        c.Obfs,
		Bandwidth:   c.Bandwidth,
		Transport:   cloneTransport(c.Transport),
		Control:     c.Control,
		Client:      c.Client,
		Credentials: c.Credentials,
		Mappings:    append([]Mapping(nil), c.Mappings...),
		Proxy:       proxy,
	}}
}

func (s ClientServerConfig) clone() ClientServerConfig {
	s.Transport = cloneTransport(s.Transport)
	s.Mappings = append([]Mapping(nil), s.Mappings...)
	return s
}

func cloneTransport(value *ClientTransportConfig) *ClientTransportConfig {
	if value == nil {
		return nil
	}
	out := *value
	if value.UDP != nil {
		udp := *value.UDP
		out.UDP = &udp
	}
	return &out
}

// ToClientConfig builds the single-server worker configuration for this server.
func (s ClientServerConfig) ToClientConfig(web WebConfig, log LogConfig, path string) *ClientConfig {
	return &ClientConfig{
		Server:      s.Server,
		Auth:        s.Auth,
		TLS:         s.TLS,
		Obfs:        s.Obfs,
		Bandwidth:   s.Bandwidth,
		Transport:   cloneTransport(s.Transport),
		Control:     s.Control,
		Client:      s.Client,
		Mappings:    append([]Mapping(nil), s.Mappings...),
		Proxy:       s.Proxy,
		Web:         web,
		Log:         log,
		Credentials: s.Credentials,
		ConfigPath:  path,
	}
}

func (c *ClientConfig) validateMultiServer() error {
	if c.legacyFieldsSet() {
		return fmt.Errorf("servers cannot be combined with legacy single-server fields")
	}
	if err := validateWeb(&c.Web); err != nil {
		return err
	}
	if err := validateLogLevel(c.Log.Level); err != nil {
		return err
	}
	names := make(map[string]struct{}, len(c.Servers))
	for i := range c.Servers {
		server := &c.Servers[i]
		server.applyDefaults()
		if _, exists := names[server.Name]; exists {
			return fmt.Errorf("servers[%d].name %q is duplicated", i, server.Name)
		}
		names[server.Name] = struct{}{}
		if err := server.validate(i); err != nil {
			return err
		}
	}
	if err := validateProxyConflicts(c.Servers, c.Web.Listen); err != nil {
		return err
	}
	return nil
}

func (c *ClientConfig) legacyFieldsSet() bool {
	return c.Server != "" || c.Auth != "" || c.TLS != (ClientTLSConfig{}) ||
		c.Obfs != (ObfsConfig{}) || c.Bandwidth != (BandwidthConfig{}) ||
		c.Transport != nil || c.Control != (ControlConfig{}) ||
		c.Client != (ClientIdentity{}) || len(c.Mappings) > 0 ||
		c.Proxy != (ProxyConfig{}) || c.Credentials != (CredentialsConfig{})
}

func (s *ClientServerConfig) applyDefaults() {
	if s.Credentials.Generation == 0 {
		s.Credentials.Generation = 1
	}
	if s.Client.Name == "" {
		s.Client.Name = s.Name
	}
}

func (s ClientServerConfig) validate(index int) error {
	field := fmt.Sprintf("servers[%d]", index)
	if s.Name == "" {
		return fmt.Errorf("%s.name is required", field)
	}
	if len(s.Name) > 64 {
		return fmt.Errorf("%s.name must not exceed 64 characters", field)
	}
	for _, r := range s.Name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("%s.name contains an invalid character", field)
	}
	if _, _, hopping, err := parseClientServer(s.Server); err != nil {
		return fmt.Errorf("%s.server: %w", field, err)
	} else if _, _, err := s.hopInterval(); err != nil {
		return fmt.Errorf("%s.transport: %w", field, err)
	} else if !hopping && hasHopInterval(s.Transport) {
		return fmt.Errorf("%s.transport.udp hop interval requires a multi-port server address", field)
	}
	if s.Auth == "" {
		return fmt.Errorf("%s.auth is required", field)
	}
	if s.Control.Token == "" {
		return fmt.Errorf("%s.control.token is required", field)
	}
	if len(s.Control.Token) < 16 {
		return fmt.Errorf("%s.control.token must be at least 16 bytes", field)
	}
	if s.Credentials.Generation < 1 {
		return fmt.Errorf("%s.credentials.generation must be at least 1", field)
	}
	if s.Client.Revision < 0 {
		return fmt.Errorf("%s.client.revision must not be negative", field)
	}
	if err := validateMappings(s.Mappings); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if err := validateProxyConfig(field+".proxy", s.Proxy); err != nil {
		return err
	}
	return nil
}

func (s ClientServerConfig) hopInterval() (int64, int64, error) {
	cfg := ClientConfig{Server: s.Server, Transport: s.Transport}
	min, max, err := cfg.HopInterval()
	return int64(min), int64(max), err
}

func hasHopInterval(transport *ClientTransportConfig) bool {
	if transport == nil || transport.UDP == nil {
		return false
	}
	udp := transport.UDP
	return udp.HopInterval != 0 || udp.MinHopInterval != 0 || udp.MaxHopInterval != 0
}

func validateProxyConfig(field string, proxy ProxyConfig) error {
	if proxy.SOCKS5 != "" {
		if err := validateProxyListen(field+".socks5", proxy.SOCKS5); err != nil {
			return err
		}
	}
	if err := validateProxyAuth(field+".socks5_auth", proxy.SOCKS5Auth); err != nil {
		return err
	}
	if proxy.HTTP != "" {
		if err := validateProxyListen(field+".http", proxy.HTTP); err != nil {
			return err
		}
	}
	if err := validateProxyAuth(field+".http_auth", proxy.HTTPAuth); err != nil {
		return err
	}
	if proxy.Enabled && proxy.SOCKS5 == "" && proxy.HTTP == "" {
		return fmt.Errorf("%s.enabled requires socks5 or http", field)
	}
	return nil
}

func validateProxyListen(field, value string) error {
	return validateHostPort(field, value, false, false)
}

func validateProxyConflicts(servers []ClientServerConfig, webListen string) error {
	seen := make(map[string]string)
	for i, server := range servers {
		if !server.Proxy.Active() {
			continue
		}
		for _, value := range []struct {
			field string
			addr  string
		}{
			{"socks5", server.Proxy.SOCKS5},
			{"http", server.Proxy.HTTP},
		} {
			if value.addr == "" {
				continue
			}
			key, err := proxyListenKey(value.addr)
			if err != nil {
				return fmt.Errorf("servers[%d].proxy.%s: %w", i, value.field, err)
			}
			if previous, exists := seen[key]; exists {
				return fmt.Errorf("%w: %s is used by %s and servers[%d]", ErrProxyConflict, key, previous, i)
			}
			seen[key] = fmt.Sprintf("servers[%d]", i)
		}
	}
	if webListen == "" {
		return nil
	}
	key, err := proxyListenKey(webListen)
	if err != nil {
		return fmt.Errorf("web.listen: %w", err)
	}
	if previous, exists := seen[key]; exists {
		return fmt.Errorf("%w: %s conflicts with web.listen used by %s", ErrProxyConflict, key, previous)
	}
	return nil
}

func proxyListenKey(value string) (string, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	return net.JoinHostPort(host, port), nil
}
