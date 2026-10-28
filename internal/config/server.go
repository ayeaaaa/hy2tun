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
	// Credentials tracks the credential generation and previous pair.
	Credentials CredentialsConfig `yaml:"credentials" json:"credentials"`

	// ConfigPath is the file this configuration was loaded from or saved to.
	// It is runtime metadata and is never serialized.
	ConfigPath string `yaml:"-" json:"-"`
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
	cfg.ConfigPath = path
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
	cfg.ConfigPath = path
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
	if err := c.validateCredentials(); err != nil {
		return err
	}
	if err := validatePortRanges("ports.allow", c.Ports.Allow); err != nil {
		return err
	}
	if err := validatePortRanges("ports.deny", c.Ports.Deny); err != nil {
		return err
	}
	if err := validatePortRanges("ports.auto_range", []string{c.Ports.AutoRange}); err != nil {
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
	if c.Credentials.Generation == 0 {
		c.Credentials.Generation = 1
	}
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

func (c *ServerConfig) validateCredentials() error {
	creds := &c.Credentials
	if creds.Generation < 1 {
		return fmt.Errorf("credentials.generation must be at least 1")
	}
	if creds.PreviousGeneration < 0 || creds.PreviousGeneration >= creds.Generation {
		return fmt.Errorf("credentials.previous_generation must be 0 or less than credentials.generation")
	}
	if creds.PreviousGeneration > 0 {
		if creds.PreviousAuth == "" {
			return fmt.Errorf("credentials.previous_auth is required when credentials.previous_generation is set")
		}
		if creds.PreviousControlToken == "" {
			return fmt.Errorf("credentials.previous_control_token is required when credentials.previous_generation is set")
		}
		return nil
	}
	if creds.PreviousAuth != "" {
		return fmt.Errorf("credentials.previous_auth must be empty when credentials.previous_generation is 0")
	}
	if creds.PreviousControlToken != "" {
		return fmt.Errorf("credentials.previous_control_token must be empty when credentials.previous_generation is 0")
	}
	return nil
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
