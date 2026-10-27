package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultClientWebListen is the conventional LAN management address for the
// client console. An empty web.listen keeps the console disabled; deployments
// that want LAN management should set this value explicitly.
const DefaultClientWebListen = "0.0.0.0:7501"

// ClientConfig is the hy2tun configuration.
type ClientConfig struct {
	Server    string          `yaml:"server" json:"server"`
	Auth      string          `yaml:"auth" json:"-"`
	TLS       ClientTLSConfig `yaml:"tls" json:"tls"`
	Obfs      ObfsConfig      `yaml:"obfs" json:"obfs"`
	Bandwidth BandwidthConfig `yaml:"bandwidth" json:"bandwidth"`
	Control   ControlConfig   `yaml:"control" json:"control"`
	Client    ClientIdentity  `yaml:"client" json:"client"`
	Mappings  []Mapping       `yaml:"mappings" json:"mappings"`
	Web       WebConfig       `yaml:"web" json:"web"`
	Proxy     ProxyConfig     `yaml:"proxy" json:"proxy"`
	Log       LogConfig       `yaml:"log" json:"log"`

	// ConfigPath is the file this configuration was loaded from or saved to.
	// It is runtime metadata and is never serialized.
	ConfigPath string `yaml:"-" json:"-"`
}

// WebEnabled reports whether the client web console is configured. The console
// is optional and stays disabled when web.listen is empty.
func (c *ClientConfig) WebEnabled() bool {
	return c != nil && c.Web.Listen != ""
}

// LoadClient reads, parses, and validates a client config file.
func LoadClient(path string) (*ClientConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read client config %s: %w", path, err)
	}
	var cfg ClientConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse client config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate client config %s: %w", path, err)
	}
	cfg.ConfigPath = path
	return &cfg, nil
}

// SaveClient writes cfg atomically with mode 0600 and keeps the previous
// contents in path+".bak".
func SaveClient(path string, cfg *ClientConfig) error {
	if cfg == nil {
		return fmt.Errorf("save client config %s: config is nil", path)
	}
	cfg.applyDefaults()
	cfg.ensureMappingIDs()
	if err := saveAtomic(path, cfg); err != nil {
		return fmt.Errorf("save client config %s: %w", path, err)
	}
	cfg.ConfigPath = path
	return nil
}

// Validate checks the client configuration and fills in documented defaults.
func (c *ClientConfig) Validate() error {
	c.applyDefaults()
	c.ensureMappingIDs()

	if err := validateHostPort("server", c.Server, false, false); err != nil {
		return err
	}
	if c.Auth == "" {
		return fmt.Errorf("auth is required")
	}
	if c.Control.Token == "" {
		return fmt.Errorf("control.token is required")
	}
	if len(c.Control.Token) < 16 {
		return fmt.Errorf("control.token must be at least 16 bytes")
	}
	if c.Client.Revision < 0 {
		return fmt.Errorf("client.revision must not be negative")
	}
	if err := validateMappings(c.Mappings); err != nil {
		return err
	}
	if err := validateWeb(&c.Web); err != nil {
		return err
	}
	if err := validateLogLevel(c.Log.Level); err != nil {
		return err
	}
	if c.Proxy.SOCKS5 != "" {
		if err := validateHostPort("proxy.socks5", c.Proxy.SOCKS5, false, false); err != nil {
			return err
		}
	}
	if err := validateProxyAuth("proxy.socks5_auth", c.Proxy.SOCKS5Auth); err != nil {
		return err
	}
	if c.Proxy.HTTP != "" {
		if err := validateHostPort("proxy.http", c.Proxy.HTTP, false, false); err != nil {
			return err
		}
	}
	if err := validateProxyAuth("proxy.http_auth", c.Proxy.HTTPAuth); err != nil {
		return err
	}
	return nil
}

func (c *ClientConfig) applyDefaults() {
	if c.Log.Level == "" {
		c.Log.Level = defaultLogLevel
	}
	if c.Web.Listen != "" && c.Web.Username == "" {
		c.Web.Username = "admin"
	}
}

func (c *ClientConfig) ensureMappingIDs() {
	for i := range c.Mappings {
		if c.Mappings[i].ID == "" {
			c.Mappings[i].ID = newMappingID()
		}
	}
}

func newMappingID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("m-%08x", time.Now().UnixNano()&0xffffffff)
	}
	return "m-" + hex.EncodeToString(b[:])
}
