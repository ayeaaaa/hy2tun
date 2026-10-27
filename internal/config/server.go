package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ServerConfig is the hy2tund configuration.
type ServerConfig struct {
	Listen     string          `yaml:"listen" json:"listen"`
	ListenHost string          `yaml:"listen_host" json:"listen_host"`
	Auth       AuthConfig      `yaml:"auth" json:"auth"`
	TLS        TLSConfig       `yaml:"tls" json:"tls"`
	Obfs       ObfsConfig      `yaml:"obfs" json:"obfs"`
	Bandwidth  BandwidthConfig `yaml:"bandwidth" json:"bandwidth"`
	QUIC       QUICConfig      `yaml:"quic" json:"quic"`
	Control    ControlConfig   `yaml:"control" json:"control"`
	Ports      PortsConfig     `yaml:"ports" json:"ports"`
	Web        WebConfig       `yaml:"web" json:"web"`
	StateFile  string          `yaml:"state_file" json:"state_file"`
	Log        LogConfig       `yaml:"log" json:"log"`
}

// LoadServer reads, parses, and validates a server config file.
func LoadServer(path string) (*ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read server config %s: %w", path, err)
	}
	var cfg ServerConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse server config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate server config %s: %w", path, err)
	}
	return &cfg, nil
}

// SaveServer writes cfg atomically with mode 0600 and keeps the previous
// contents in path+".bak".
func SaveServer(path string, cfg *ServerConfig) error {
	if cfg == nil {
		return fmt.Errorf("save server config %s: config is nil", path)
	}
	cfg.applyDefaults()
	if err := saveAtomic(path, cfg); err != nil {
		return fmt.Errorf("save server config %s: %w", path, err)
	}
	return nil
}

// Validate checks the server configuration and fills in documented defaults.
func (c *ServerConfig) Validate() error {
	c.applyDefaults()

	if err := validateHostPort("listen", c.Listen, true, true); err != nil {
		return err
	}
	if err := c.Auth.validate(); err != nil {
		return err
	}
	if (c.TLS.Cert == "") != (c.TLS.Key == "") {
		return fmt.Errorf("tls.cert and tls.key must be set together")
	}
	if len(c.Control.Token) < 16 {
		return fmt.Errorf("control.token must be at least 16 bytes")
	}
	for _, spec := range c.Ports.Allow {
		if err := validatePortSpec("ports.allow", spec); err != nil {
			return err
		}
	}
	for _, spec := range c.Ports.Deny {
		if err := validatePortSpec("ports.deny", spec); err != nil {
			return err
		}
	}
	if err := validatePortSpec("ports.auto_range", c.Ports.AutoRange); err != nil {
		return err
	}
	if err := validateWeb(&c.Web); err != nil {
		return err
	}
	if err := validateLogLevel(c.Log.Level); err != nil {
		return err
	}
	if c.Ports.OfflineReserve < 0 {
		return fmt.Errorf("ports.offline_reserve must not be negative")
	}
	return nil
}

func (c *ServerConfig) applyDefaults() {
	if c.Auth.Type == "" && c.Auth.Password != "" {
		c.Auth.Type = "password"
	}
	if c.Ports.AutoRange == "" {
		c.Ports.AutoRange = defaultAutoRange
	}
	if c.Ports.OfflineReserve == 0 {
		c.Ports.OfflineReserve = defaultOfflineReserve
	}
	if c.Log.Level == "" {
		c.Log.Level = defaultLogLevel
	}
	if c.Web.Listen != "" && c.Web.Username == "" {
		c.Web.Username = "admin"
	}
}

func (a *AuthConfig) validate() error {
	switch a.Type {
	case "password":
		if a.Password == "" {
			return fmt.Errorf("auth.password is required for password authentication")
		}
	case "userpass":
		if len(a.Users) == 0 {
			return fmt.Errorf("auth.users must contain at least one user for userpass authentication")
		}
		for user, pass := range a.Users {
			if user == "" {
				return fmt.Errorf("auth.users must not contain an empty username")
			}
			if pass == "" {
				return fmt.Errorf("auth.users[%s] must not have an empty password", user)
			}
		}
	default:
		return fmt.Errorf("auth.type must be \"password\" or \"userpass\"")
	}
	return nil
}
