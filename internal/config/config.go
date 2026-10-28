// Package config defines the on-disk configuration format shared by the
// hy2tun client and hy2tund server.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"hy2tun/internal/ports"

	"gopkg.in/yaml.v3"
)

// Mapping describes one public port forwarded to a client-local address.
type Mapping struct {
	ID         string `yaml:"id" json:"id"`
	Type       string `yaml:"type" json:"type"`
	RemotePort int    `yaml:"remote_port" json:"remote_port"`
	LocalAddr  string `yaml:"local_addr" json:"local_addr"`
	Note       string `yaml:"note" json:"note"`
	Enabled    bool   `yaml:"enabled" json:"enabled"`
}

// ControlConfig holds the shared secret used to authenticate the control
// stream. The token is never logged.
type ControlConfig struct {
	Token string `yaml:"token" json:"-"`
}

// AuthConfig configures HY2 authentication on the server.
type AuthConfig struct {
	Type     string            `yaml:"type" json:"type"`
	Password string            `yaml:"password,omitempty" json:"-"`
	Users    map[string]string `yaml:"users,omitempty" json:"-"`
}

// CredentialsConfig tracks the generation of the current credential pair and
// the previous pair retained during rotation. Previous secrets persist in YAML
// so old credentials keep working across restarts, but are never exposed
// through JSON APIs.
type CredentialsConfig struct {
	Generation           int64  `yaml:"generation" json:"generation"`
	PreviousGeneration   int64  `yaml:"previous_generation" json:"previous_generation"`
	PreviousAuth         string `yaml:"previous_auth,omitempty" json:"-"`
	PreviousControlToken string `yaml:"previous_control_token,omitempty" json:"-"`
}

// TLSConfig is the server-side TLS certificate pair.
type TLSConfig struct {
	Cert string `yaml:"cert" json:"cert"`
	Key  string `yaml:"key" json:"-"`
}

// ClientTLSConfig configures how the client verifies the server certificate.
type ClientTLSConfig struct {
	SNI       string `yaml:"sni" json:"sni"`
	Insecure  bool   `yaml:"insecure" json:"insecure"`
	PinSHA256 string `yaml:"pin_sha256" json:"pin_sha256"`
	CA        string `yaml:"ca" json:"ca"`
}

// ObfsConfig configures HY2 obfuscation.
type ObfsConfig struct {
	Type       string           `yaml:"type" json:"type"`
	Salamander SalamanderConfig `yaml:"salamander" json:"salamander"`
}

// SalamanderConfig configures the Salamander obfuscator.
type SalamanderConfig struct {
	Password string `yaml:"password,omitempty" json:"-"`
}

// BandwidthConfig configures HY2 bandwidth limits. Zero means unlimited.
type BandwidthConfig struct {
	Up           Bandwidth `yaml:"up" json:"up"`
	Down         Bandwidth `yaml:"down" json:"down"`
	IgnoreClient bool      `yaml:"ignore_client" json:"ignore_client"`
}

// QUICConfig configures QUIC transport limits. Window values are byte sizes.
type QUICConfig struct {
	InitialStreamReceiveWindow     uint64   `yaml:"initial_stream_receive_window" json:"initial_stream_receive_window"`
	MaxStreamReceiveWindow         uint64   `yaml:"max_stream_receive_window" json:"max_stream_receive_window"`
	InitialConnectionReceiveWindow uint64   `yaml:"initial_connection_receive_window" json:"initial_connection_receive_window"`
	MaxConnectionReceiveWindow     uint64   `yaml:"max_connection_receive_window" json:"max_connection_receive_window"`
	MaxIdleTimeout                 Duration `yaml:"max_idle_timeout" json:"max_idle_timeout"`
	MaxIncomingStreams             int64    `yaml:"max_incoming_streams" json:"max_incoming_streams"`
}

// UnmarshalYAML accepts both raw byte counts and strings such as "8 mb" for
// the receive-window fields.
func (q *QUICConfig) UnmarshalYAML(value *yaml.Node) error {
	type rawQUIC struct {
		InitialStreamReceiveWindow     ByteSize `yaml:"initial_stream_receive_window"`
		MaxStreamReceiveWindow         ByteSize `yaml:"max_stream_receive_window"`
		InitialConnectionReceiveWindow ByteSize `yaml:"initial_connection_receive_window"`
		MaxConnectionReceiveWindow     ByteSize `yaml:"max_connection_receive_window"`
		MaxIdleTimeout                 Duration `yaml:"max_idle_timeout"`
		MaxIncomingStreams             int64    `yaml:"max_incoming_streams"`
	}
	var raw rawQUIC
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*q = QUICConfig{
		InitialStreamReceiveWindow:     uint64(raw.InitialStreamReceiveWindow),
		MaxStreamReceiveWindow:         uint64(raw.MaxStreamReceiveWindow),
		InitialConnectionReceiveWindow: uint64(raw.InitialConnectionReceiveWindow),
		MaxConnectionReceiveWindow:     uint64(raw.MaxConnectionReceiveWindow),
		MaxIdleTimeout:                 raw.MaxIdleTimeout,
		MaxIncomingStreams:             raw.MaxIncomingStreams,
	}
	return nil
}

// PortsConfig configures which public ports clients may request.
type PortsConfig struct {
	Allow          []string `yaml:"allow" json:"allow"`
	Deny           []string `yaml:"deny" json:"deny"`
	AutoRange      string   `yaml:"auto_range" json:"auto_range"`
	OfflineReserve Duration `yaml:"offline_reserve" json:"offline_reserve"`
}

// WebTLSConfig optionally enables HTTPS for a web UI.
type WebTLSConfig struct {
	Cert string `yaml:"cert" json:"cert"`
	Key  string `yaml:"key" json:"-"`
}

// WebConfig configures a web management listener.
type WebConfig struct {
	Listen       string       `yaml:"listen" json:"listen"`
	Username     string       `yaml:"username" json:"username"`
	PasswordHash string       `yaml:"password_hash" json:"-"`
	TLS          WebTLSConfig `yaml:"tls" json:"tls"`
	// TrustProxy enables X-Forwarded-For client-IP extraction for the login
	// rate limiter. It must only be enabled when a trusted reverse proxy
	// overwrites the header; otherwise clients can spoof their IP.
	TrustProxy bool `yaml:"trust_proxy" json:"trust_proxy"`
}

// LogConfig configures structured logging.
type LogConfig struct {
	Level string `yaml:"level" json:"level"`
	File  string `yaml:"file" json:"file"`
}

// ClientIdentity is the persistent identity of a client.
type ClientIdentity struct {
	ID       string `yaml:"id" json:"id"`
	Name     string `yaml:"name" json:"name"`
	Revision int64  `yaml:"revision" json:"revision"`
}

// ProxyConfig configures the optional local SOCKS5/HTTP proxy listeners.
type ProxyConfig struct {
	SOCKS5     string `yaml:"socks5" json:"socks5"`
	SOCKS5Auth string `yaml:"socks5_auth,omitempty" json:"-"`
	HTTP       string `yaml:"http" json:"http"`
	HTTPAuth   string `yaml:"http_auth,omitempty" json:"-"`
}

const (
	defaultAutoRange      = "20000-29999"
	defaultOfflineReserve = Duration(24 * 60 * 60 * 1_000_000_000) // 24h in nanoseconds
	defaultLogLevel       = "info"
)

// validateHostPort checks that value is a host:port pair. Listen addresses may
// use an empty host (all interfaces) and port 0 (kernel-assigned port).
func validateHostPort(field, value string, allowEmptyHost, allowZeroPort bool) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	host, portStr, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("%s must be a valid host:port: %w", field, err)
	}
	if !allowEmptyHost && host == "" {
		return fmt.Errorf("%s must include a host", field)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fmt.Errorf("%s has an invalid port %q", field, portStr)
	}
	minPort := 1
	if allowZeroPort {
		minPort = 0
	}
	if port < minPort || port > 65535 {
		return fmt.Errorf("%s port must be between %d and 65535", field, minPort)
	}
	return nil
}

// validatePortRanges checks a list of port specifications with the same
// parser used by the runtime port policy.
func validatePortRanges(field string, specs []string) error {
	if _, err := ports.ParseRanges(specs); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

func validateMappings(mappings []Mapping) error {
	seen := make(map[string]struct{}, len(mappings))
	for i, m := range mappings {
		field := fmt.Sprintf("mappings[%d]", i)
		if m.ID == "" {
			return fmt.Errorf("%s.id is required", field)
		}
		if _, ok := seen[m.ID]; ok {
			return fmt.Errorf("%s.id %q is duplicated", field, m.ID)
		}
		seen[m.ID] = struct{}{}

		switch m.Type {
		case "tcp", "udp":
		default:
			return fmt.Errorf("%s.type must be \"tcp\" or \"udp\"", field)
		}
		if m.RemotePort < 0 || m.RemotePort > 65535 {
			return fmt.Errorf("%s.remote_port must be between 0 and 65535", field)
		}
		host, portStr, err := net.SplitHostPort(m.LocalAddr)
		if err != nil {
			return fmt.Errorf("%s.local_addr must be a valid host:port: %w", field, err)
		}
		if host == "" {
			return fmt.Errorf("%s.local_addr must include a host", field)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("%s.local_addr has an invalid port %q", field, portStr)
		}
	}
	return nil
}

func validateLogLevel(level string) error {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug", "info", "warn", "error":
		return nil
	default:
		return fmt.Errorf("log.level must be one of debug, info, warn, error")
	}
}

func validateWeb(web *WebConfig) error {
	if web.Listen != "" {
		if err := validateHostPort("web.listen", web.Listen, true, true); err != nil {
			return err
		}
	}
	if (web.TLS.Cert == "") != (web.TLS.Key == "") {
		return fmt.Errorf("web.tls.cert and web.tls.key must be set together")
	}
	return nil
}

func validateProxyAuth(field, value string) error {
	if value == "" {
		return nil
	}
	user, pass, ok := strings.Cut(value, ":")
	if !ok || user == "" {
		return fmt.Errorf("%s must be in user:password form", field)
	}
	if pass == "" {
		return fmt.Errorf("%s must include a password", field)
	}
	return nil
}

// writeFileAtomic writes data to path through a temporary file, fsyncs it,
// and renames it into place. The file mode is always 0600.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer os.Remove(tmpName)

	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		// Windows does not support flushing directory handles. The rename is
		// still atomic; only the extra durability barrier is unavailable.
		if runtime.GOOS == "windows" {
			return nil
		}
		return err
	}
	return nil
}

// saveAtomic marshals and writes a config, keeping the previous file as .bak.
func saveAtomic(path string, value any) error {
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := writeFileAtomic(path+".bak", old); err != nil {
			return fmt.Errorf("write backup %s: %w", path+".bak", err)
		}
	case os.IsNotExist(err):
	default:
		return fmt.Errorf("read previous config %s: %w", path, err)
	}
	if err := writeFileAtomic(path, data); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}
