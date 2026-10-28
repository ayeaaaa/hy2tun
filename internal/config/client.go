package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"hy2tun/internal/ports"

	"gopkg.in/yaml.v3"
)

// Mapping edit errors are stable so API layers can map them to status codes
// without matching error strings.
var (
	ErrMappingNotFound = errors.New("mapping not found")
	ErrMappingExists   = errors.New("mapping already exists")
)

// DefaultClientWebListen is the conventional LAN management address for the
// client console. An empty web.listen keeps the console disabled; deployments
// that want LAN management should set this value explicitly.
const DefaultClientWebListen = "0.0.0.0:7501"

// ClientConfig is the hy2tun configuration.
type ClientConfig struct {
	Server    string                 `yaml:"server" json:"server"`
	Auth      string                 `yaml:"auth" json:"-"`
	TLS       ClientTLSConfig        `yaml:"tls" json:"tls"`
	Obfs      ObfsConfig             `yaml:"obfs" json:"obfs"`
	Bandwidth BandwidthConfig        `yaml:"bandwidth" json:"bandwidth"`
	Transport *ClientTransportConfig `yaml:"transport,omitempty" json:"transport,omitempty"`
	Control   ControlConfig          `yaml:"control" json:"control"`
	Client    ClientIdentity         `yaml:"client" json:"client"`
	Mappings  []Mapping              `yaml:"mappings" json:"mappings"`
	Web       WebConfig              `yaml:"web" json:"web"`
	Proxy     ProxyConfig            `yaml:"proxy" json:"proxy"`
	Log       LogConfig              `yaml:"log" json:"log"`
	// Credentials tracks the credential generation and previous pair.
	Credentials CredentialsConfig `yaml:"credentials" json:"credentials"`

	// ConfigPath is the file this configuration was loaded from or saved to.
	// It is runtime metadata and is never serialized.
	ConfigPath string `yaml:"-" json:"-"`
}

// ClientTransportConfig configures optional client transport behavior. It is
// omitted from serialized configs when unset so single-port configs stay
// unchanged.
type ClientTransportConfig struct {
	UDP *ClientTransportUDPConfig `yaml:"udp,omitempty" json:"udp,omitempty"`
}

// ClientTransportUDPConfig configures UDP port hopping. HopInterval is a fixed
// interval; MinHopInterval and MaxHopInterval select a random interval in that
// range. The two forms are mutually exclusive.
type ClientTransportUDPConfig struct {
	HopInterval    Duration `yaml:"hop_interval,omitempty" json:"hop_interval,omitempty"`
	MinHopInterval Duration `yaml:"min_hop_interval,omitempty" json:"min_hop_interval,omitempty"`
	MaxHopInterval Duration `yaml:"max_hop_interval,omitempty" json:"max_hop_interval,omitempty"`
}

// WebEnabled reports whether the client web console is configured. The console
// is optional and stays disabled when web.listen is empty.
func (c *ClientConfig) WebEnabled() bool {
	return c != nil && c.Web.Listen != ""
}

// ProxyEnabled reports whether at least one local proxy listener is configured.
// Proxy listeners are opt-in, so an empty proxy section leaves both disabled.
func (c *ClientConfig) ProxyEnabled() bool {
	return c != nil && (c.Proxy.SOCKS5 != "" || c.Proxy.HTTP != "")
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

	_, _, hopping, err := parseClientServer(c.Server)
	if err != nil {
		return err
	}
	minHop, maxHop, err := c.HopInterval()
	if err != nil {
		return err
	}
	if !hopping && (minHop != 0 || maxHop != 0) {
		return fmt.Errorf("transport.udp hop interval requires a multi-port server address")
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
	if c.Credentials.Generation < 1 {
		return fmt.Errorf("credentials.generation must be at least 1")
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

// parseClientServer splits a server address into its host and port ranges and
// reports whether the address covers more than one unique port.
func parseClientServer(value string) (string, []ports.Range, bool, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil, false, fmt.Errorf("server is required")
	}
	host, portStr, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return "", nil, false, fmt.Errorf("server must be a valid host:port or port range: %w", err)
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return "", nil, false, fmt.Errorf("server must include a host")
	}
	ranges, err := ports.ParseRanges(strings.Split(portStr, ","))
	if err != nil {
		return "", nil, false, fmt.Errorf("server: %w", err)
	}
	return host, ranges, uniquePortCount(ranges) > 1, nil
}

func uniquePortCount(ranges []ports.Range) int {
	seen := make(map[int]struct{})
	for _, r := range ranges {
		for port := r.Start; port <= r.End; port++ {
			seen[port] = struct{}{}
		}
	}
	return len(seen)
}

// ServerPortHopping reports whether the configured server address covers more
// than one unique port.
func (c *ClientConfig) ServerPortHopping() bool {
	_, _, hopping, err := parseClientServer(c.Server)
	return err == nil && hopping
}

// NormalizedServer returns the server address to dial and whether it uses port
// hopping. Addresses that normalize to a single unique port are returned as a
// canonical host:port pair.
func (c *ClientConfig) NormalizedServer() (string, bool, error) {
	host, ranges, hopping, err := parseClientServer(c.Server)
	if err != nil {
		return "", false, err
	}
	if hopping {
		return c.Server, true, nil
	}
	if len(ranges) == 0 {
		return "", false, fmt.Errorf("server must include a port")
	}
	return net.JoinHostPort(host, strconv.Itoa(ranges[0].Start)), false, nil
}

// HopInterval returns the configured UDP hop interval bounds. A zero, zero
// result means no interval is configured and the runtime default applies.
func (c *ClientConfig) HopInterval() (time.Duration, time.Duration, error) {
	if c.Transport == nil || c.Transport.UDP == nil {
		return 0, 0, nil
	}
	udp := c.Transport.UDP
	fixed := time.Duration(udp.HopInterval)
	min := time.Duration(udp.MinHopInterval)
	max := time.Duration(udp.MaxHopInterval)

	if fixed != 0 && (min != 0 || max != 0) {
		return 0, 0, fmt.Errorf("hop_interval cannot be used together with min_hop_interval or max_hop_interval")
	}
	if min == 0 && max == 0 {
		if fixed == 0 {
			return 0, 0, nil
		}
		if fixed < 5*time.Second {
			return 0, 0, fmt.Errorf("hop interval must be at least 5s")
		}
		return fixed, fixed, nil
	}
	if min == 0 || max == 0 {
		return 0, 0, fmt.Errorf("min_hop_interval and max_hop_interval must both be set")
	}
	if min < 5*time.Second || max < 5*time.Second {
		return 0, 0, fmt.Errorf("hop interval must be at least 5s")
	}
	if min > max {
		return 0, 0, fmt.Errorf("min_hop_interval must not be greater than max_hop_interval")
	}
	return min, max, nil
}

func (c *ClientConfig) applyDefaults() {
	if c.Credentials.Generation == 0 {
		c.Credentials.Generation = 1
	}
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
