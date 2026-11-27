package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultListen          = "127.0.0.1:7600"
	defaultBasePath        = "/"
	defaultStateFile       = "/var/lib/hy2tun-manager/state.json"
	defaultKeyFile         = "/var/lib/hy2tun-manager/manager.key"
	defaultAuditFile       = "/var/lib/hy2tun-manager/audit.jsonl"
	defaultRefreshInterval = 30 * time.Second
	minRefreshInterval     = time.Second
)

// Config is the manager's YAML configuration. Paths are kept as written so
// startup errors show the operator's own values.
type Config struct {
	Listen          string
	BasePath        string
	StateFile       string
	KeyFile         string
	AuditFile       string
	RefreshInterval time.Duration
	TrustProxy      bool
}

// fileConfig mirrors Config with pointers for scalar fields whose "absent"
// and "explicitly empty" values must be distinguished.
type fileConfig struct {
	Listen          *string `yaml:"listen"`
	BasePath        *string `yaml:"base_path"`
	StateFile       *string `yaml:"state_file"`
	KeyFile         *string `yaml:"key_file"`
	AuditFile       *string `yaml:"audit_file"`
	RefreshInterval *string `yaml:"refresh_interval"`
	TrustProxy      bool    `yaml:"trust_proxy"`
}

// LoadConfig reads a manager YAML configuration and applies defaults for the
// listen address, base path, state/key/audit paths, refresh interval, and
// trust-proxy switch.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read manager config %s: %w", path, err)
	}

	var raw fileConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, fmt.Errorf("parse manager config %s: %w", path, err)
	}

	cfg := Config{
		Listen:          defaultListen,
		BasePath:        defaultBasePath,
		StateFile:       defaultStateFile,
		KeyFile:         defaultKeyFile,
		AuditFile:       defaultAuditFile,
		RefreshInterval: defaultRefreshInterval,
		TrustProxy:      raw.TrustProxy,
	}
	if raw.Listen != nil {
		cfg.Listen = *raw.Listen
	}
	if raw.BasePath != nil {
		cfg.BasePath = *raw.BasePath
	}
	if raw.StateFile != nil {
		cfg.StateFile = *raw.StateFile
	}
	if raw.KeyFile != nil {
		cfg.KeyFile = *raw.KeyFile
	}
	if raw.AuditFile != nil {
		cfg.AuditFile = *raw.AuditFile
	}
	if raw.RefreshInterval != nil {
		interval, err := time.ParseDuration(*raw.RefreshInterval)
		if err != nil {
			return Config{}, fmt.Errorf("manager config %s: refresh_interval: %w", path, err)
		}
		cfg.RefreshInterval = interval
	}

	cfg.Listen = strings.TrimSpace(cfg.Listen)
	cfg.BasePath = strings.TrimSpace(cfg.BasePath)
	cfg.StateFile = strings.TrimSpace(cfg.StateFile)
	cfg.KeyFile = strings.TrimSpace(cfg.KeyFile)
	cfg.AuditFile = strings.TrimSpace(cfg.AuditFile)
	if cfg.Listen == "" {
		return Config{}, fmt.Errorf("manager config %s: listen must not be empty", path)
	}
	if cfg.StateFile == "" {
		return Config{}, fmt.Errorf("manager config %s: state_file must not be empty", path)
	}
	if cfg.KeyFile == "" {
		return Config{}, fmt.Errorf("manager config %s: key_file must not be empty", path)
	}
	if cfg.AuditFile == "" {
		return Config{}, fmt.Errorf("manager config %s: audit_file must not be empty", path)
	}
	if cfg.RefreshInterval < minRefreshInterval {
		return Config{}, fmt.Errorf("manager config %s: refresh_interval must be at least %s", path, minRefreshInterval)
	}
	if err := validateConfigPaths(cfg); err != nil {
		return Config{}, fmt.Errorf("manager config %s: %w", path, err)
	}
	cfg.BasePath = normalizeBasePath(cfg.BasePath)
	return cfg, nil
}

// validateConfigPaths rejects configurations where two roles would share a
// file, which would let state writes corrupt the key or audit log.
func validateConfigPaths(cfg Config) error {
	paths := []struct {
		name string
		path string
	}{
		{"state_file", cfg.StateFile},
		{"key_file", cfg.KeyFile},
		{"audit_file", cfg.AuditFile},
	}
	seen := make(map[string]string, len(paths))
	for _, entry := range paths {
		cleaned := filepath.Clean(entry.path)
		if previous, exists := seen[cleaned]; exists {
			return fmt.Errorf("%s and %s must not point to the same file", previous, entry.name)
		}
		seen[cleaned] = entry.name
	}
	return nil
}

// normalizeBasePath makes a reverse-proxy prefix consistently start and end
// with a slash. An empty value means the root path.
func normalizeBasePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "/"
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	if !strings.HasSuffix(value, "/") {
		value += "/"
	}
	return value
}
