package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

// BasicErrorCode is a stable error category for the basic configuration API.
// CLI and LuCI layers map these values to exit codes and HTTP statuses.
type BasicErrorCode string

const (
	BasicCodeParse          BasicErrorCode = "parse_error"
	BasicCodeValidation     BasicErrorCode = "validation"
	BasicCodeConflict       BasicErrorCode = "conflict"
	BasicCodeUnknownFields  BasicErrorCode = "unknown_fields"
	BasicCodeMulti          BasicErrorCode = "multi"
	BasicCodeAdvanced       BasicErrorCode = "advanced"
	BasicCodeServiceRunning BasicErrorCode = "service_running"
	BasicCodeInternal       BasicErrorCode = "internal"
)

// BasicError reports a basic configuration failure with a stable code.
type BasicError struct {
	Code     BasicErrorCode
	Message  string
	Advanced bool
}

// Error implements error.
func (e *BasicError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return string(e.Code)
}

// BasicConfig is the read model for the single-server basic configuration
// form. It never contains secrets; secret-bearing fields are reported as
// booleans. Values are the raw YAML values, except credential_generation,
// which is normalized from 0 to 1 for display.
type BasicConfig struct {
	Exists               bool      `json:"exists"`
	Multi                bool      `json:"multi"`
	Revision             string    `json:"revision"`
	Valid                bool      `json:"valid"`
	ValidationError      string    `json:"validation_error"`
	AdvancedError        bool      `json:"advanced_error"`
	UnknownFields        bool      `json:"unknown_fields"`
	Server               string    `json:"server"`
	ServerIdentity       string    `json:"server_identity"`
	ServerLocked         bool      `json:"server_locked"`
	CredentialGeneration int64     `json:"credential_generation"`
	AuthSet              bool      `json:"auth_set"`
	ControlTokenSet      bool      `json:"control_token_set"`
	TLSSNI               string    `json:"tls_sni"`
	TLSInsecure          bool      `json:"tls_insecure"`
	ClientName           string    `json:"client_name"`
	Mappings             []Mapping `json:"mappings"`
	WebListen            string    `json:"web_listen"`
	WebUsername          string    `json:"web_username"`
	PasswordSet          bool      `json:"password_set"`
	WebTLS               bool      `json:"web_tls"`
	Advanced             bool      `json:"advanced"`
	LogLevel             string    `json:"log_level"`
	LogFile              string    `json:"log_file"`
}

// isKnownTopLevelKey reports whether key is understood by ClientConfig. Any
// other top-level key would be dropped by a save, so the basic page refuses to
// edit such files.
func isKnownTopLevelKey(key string) bool {
	switch key {
	case "server", "servers", "auth", "tls", "obfs", "bandwidth", "transport",
		"control", "client", "mappings", "web", "proxy", "log", "credentials":
		return true
	default:
		return false
	}
}

// FileRevision returns the SHA-256 hex digest of path. A missing file has an
// empty revision and no error so a new configuration can be created.
func FileRevision(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read client config %s: %w", path, err)
	}
	return contentRevision(data), nil
}

func contentRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// loadClientForEdit reads and parses a client config without validating it.
// Invalid-but-parseable configurations must stay editable so the basic form
// can repair them.
func loadClientForEdit(path string) (*ClientConfig, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var cfg ClientConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, data, &BasicError{
			Code:    BasicCodeParse,
			Message: fmt.Sprintf("parse client config %s: %v", path, err),
		}
	}
	cfg.ConfigPath = path
	return &cfg, data, nil
}

// hasUnknownTopLevelKeys reports whether the YAML document has a top-level
// key outside knownTopLevelKeys.
func hasUnknownTopLevelKeys(data []byte) bool {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false
	}
	if len(doc.Content) == 0 {
		return false
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if !isKnownTopLevelKey(root.Content[i].Value) {
			return true
		}
	}
	return false
}

// isAdvancedConfig reports whether cfg uses fields outside the basic form.
// These fields are preserved by a basic save but cannot be edited there.
func isAdvancedConfig(cfg *ClientConfig) bool {
	if cfg == nil {
		return false
	}
	if cfg.TLS.CA != "" || cfg.TLS.PinSHA256 != "" {
		return true
	}
	if cfg.Obfs != (ObfsConfig{}) {
		return true
	}
	if cfg.Bandwidth != (BandwidthConfig{}) {
		return true
	}
	if cfg.Transport != nil {
		return true
	}
	if cfg.Proxy.Enabled || cfg.Proxy.SOCKS5 != "" || cfg.Proxy.SOCKS5Auth != "" ||
		cfg.Proxy.HTTP != "" || cfg.Proxy.HTTPAuth != "" {
		return true
	}
	if cfg.Web.TLS != (WebTLSConfig{}) || cfg.Web.TrustProxy {
		return true
	}
	level := strings.TrimSpace(cfg.Log.Level)
	if level != "" && !strings.EqualFold(level, defaultLogLevel) {
		return true
	}
	return cfg.Log.File != ""
}

// serverLocked reports whether the server address is tied to advanced fields
// or rotated credentials, so the basic form must not change it.
func serverLocked(cfg *ClientConfig) bool {
	if cfg == nil {
		return false
	}
	return cfg.TLS.CA != "" || cfg.TLS.PinSHA256 != "" ||
		cfg.Obfs != (ObfsConfig{}) ||
		cfg.Bandwidth != (BandwidthConfig{}) ||
		cfg.Transport != nil ||
		cfg.Credentials.Generation > 1
}

// cloneClientConfig copies the slices and nested pointers that Validate
// mutates, keeping the caller's raw values intact.
func cloneClientConfig(cfg *ClientConfig) *ClientConfig {
	if cfg == nil {
		return nil
	}
	out := *cfg
	out.Transport = cloneTransport(cfg.Transport)
	out.Mappings = append([]Mapping(nil), cfg.Mappings...)
	if cfg.Servers != nil {
		out.Servers = make([]ClientServerConfig, len(cfg.Servers))
		for i := range cfg.Servers {
			out.Servers[i] = cfg.Servers[i].clone()
		}
	}
	return &out
}

// basicConfigCopy returns a copy with advanced fields cleared so validation can
// distinguish basic-field errors from advanced-field errors.
func basicConfigCopy(cfg *ClientConfig) *ClientConfig {
	out := cloneClientConfig(cfg)
	out.TLS.CA = ""
	out.TLS.PinSHA256 = ""
	out.Obfs = ObfsConfig{}
	out.Bandwidth = BandwidthConfig{}
	out.Transport = nil
	out.Proxy = ProxyConfig{}
	out.Web.TLS = WebTLSConfig{}
	out.Web.TrustProxy = false
	out.Log = LogConfig{}
	return out
}

// validateBasicState validates a copy of cfg and reports whether it is usable.
// It returns valid, a human-readable validation error, and whether the failure
// is confined to advanced fields. Validate mutates defaults and mapping IDs,
// so both passes run on cloned configurations.
func validateBasicState(cfg *ClientConfig) (bool, string, bool) {
	if cfg == nil {
		return false, "client config is nil", false
	}
	if cfg.IsMulti() {
		full := cloneClientConfig(cfg)
		if err := full.Validate(); err != nil {
			return false, err.Error(), false
		}
		if err := CheckClient(full); err != nil {
			return false, err.Error(), false
		}
		return true, "", false
	}

	basic := basicConfigCopy(cfg)
	if err := basic.Validate(); err != nil {
		return false, err.Error(), false
	}
	// An invalid bcrypt hash is a basic Web-console field error, not an
	// advanced-field error: the form can replace the hash with a new password.
	if err := checkWebPassword(basic.Web.PasswordHash); err != nil {
		return false, err.Error(), false
	}
	if basic.Web.Listen != "" && basic.Web.PasswordHash == "" {
		return false, "web.password_hash is required when web.listen is enabled", false
	}

	full := cloneClientConfig(cfg)
	if err := full.Validate(); err != nil {
		return false, err.Error(), true
	}
	if err := CheckClient(full); err != nil {
		return false, err.Error(), true
	}
	return true, "", false
}

// GetBasic reads the basic configuration state for path. A missing file
// returns empty-form defaults; a parse failure returns a BasicError.
func GetBasic(path string) (*BasicConfig, error) {
	cfg, data, err := loadClientForEdit(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &BasicConfig{
				ClientName:           "openwrt",
				WebListen:            DefaultClientWebListen,
				WebUsername:          "admin",
				CredentialGeneration: 1,
				Mappings:             []Mapping{},
			}, nil
		}
		var basicErr *BasicError
		if errors.As(err, &basicErr) {
			return nil, basicErr
		}
		// The CLI contract reports YAML parse and IO failures as
		// parse_error, both with exit code 1.
		return nil, &BasicError{
			Code:    BasicCodeParse,
			Message: fmt.Sprintf("read client config %s: %v", path, err),
		}
	}

	out := &BasicConfig{
		Exists:   true,
		Revision: contentRevision(data),
		Mappings: []Mapping{},
	}

	if cfg.IsMulti() {
		out.Multi = true
		out.Valid, out.ValidationError, _ = validateBasicState(cfg)
		out.WebListen = cfg.Web.Listen
		out.WebTLS = webTLSConfigured(cfg.Web)
		out.PasswordSet = cfg.Web.PasswordHash != ""
		out.LogFile = cfg.Log.File
		return out, nil
	}

	out.UnknownFields = hasUnknownTopLevelKeys(data)
	out.Server = cfg.Server
	if identity, err := ServerIdentity(cfg.Server); err == nil {
		out.ServerIdentity = identity
	}
	out.ServerLocked = serverLocked(cfg)
	out.CredentialGeneration = cfg.Credentials.Generation
	if out.CredentialGeneration == 0 {
		out.CredentialGeneration = 1
	}
	out.AuthSet = cfg.Auth != ""
	out.ControlTokenSet = cfg.Control.Token != ""
	out.TLSSNI = cfg.TLS.SNI
	out.TLSInsecure = cfg.TLS.Insecure
	out.ClientName = cfg.Client.Name
	out.Mappings = append([]Mapping{}, cfg.Mappings...)
	out.WebListen = cfg.Web.Listen
	out.WebUsername = cfg.Web.Username
	out.PasswordSet = cfg.Web.PasswordHash != ""
	out.WebTLS = webTLSConfigured(cfg.Web)
	out.Advanced = isAdvancedConfig(cfg)
	out.LogLevel = cfg.Log.Level
	out.LogFile = cfg.Log.File
	out.Valid, out.ValidationError, out.AdvancedError = validateBasicState(cfg)
	return out, nil
}

func webTLSConfigured(web WebConfig) bool {
	return web.TLS.Cert != "" || web.TLS.Key != ""
}

// BasicUpdate contains the editable basic configuration fields. Pointer fields
// distinguish a missing JSON field from an explicitly empty value; a nil
// pointer is rejected for required fields. Auth, ControlToken, and WebPassword
// preserve the existing secret when nil or empty.
type BasicUpdate struct {
	ExpectedRevision   string     `json:"expected_revision"`
	Server             *string    `json:"server"`
	Auth               *string    `json:"auth"`
	ControlToken       *string    `json:"control_token"`
	TLSSNI             *string    `json:"tls_sni"`
	TLSInsecure        *bool      `json:"tls_insecure"`
	ClientName         *string    `json:"client_name"`
	Mappings           *[]Mapping `json:"mappings"`
	WebListen          *string    `json:"web_listen"`
	WebUsername        *string    `json:"web_username"`
	WebPassword        *string    `json:"web_password"`
	WebPasswordConfirm *string    `json:"web_password_confirm"`
}

// SetBasicOptions carries hooks that run while the configuration lock is held.
type SetBasicOptions struct {
	// BeforeWrite runs inside the config lock immediately before the file is
	// written. A returned *BasicError is passed through unchanged.
	BeforeWrite func() error
}

// SetBasic validates and writes the basic fields of the client configuration at
// path, preserving advanced fields and returning the new read model.
func SetBasic(path string, update *BasicUpdate, opts SetBasicOptions) (*BasicConfig, error) {
	if err := validateBasicUpdate(update); err != nil {
		return nil, err
	}
	if err := withConfigLock(path, func() error {
		return setBasicLocked(path, update, opts)
	}); err != nil {
		var basicErr *BasicError
		if errors.As(err, &basicErr) {
			return nil, basicErr
		}
		return nil, basicInternal("write client config %s: %v", path, err)
	}
	return GetBasic(path)
}

func validateBasicUpdate(update *BasicUpdate) error {
	if update == nil {
		return basicValidation("update is required")
	}
	if update.Server == nil {
		return basicValidation("server is required")
	}
	if update.ClientName == nil {
		return basicValidation("client_name is required")
	}
	if update.Mappings == nil {
		return basicValidation("mappings is required")
	}
	if update.WebListen == nil {
		return basicValidation("web_listen is required")
	}
	if update.WebUsername == nil {
		return basicValidation("web_username is required")
	}
	return nil
}

func setBasicLocked(path string, update *BasicUpdate, opts SetBasicOptions) error {
	cfg, data, err := loadClientForEdit(path)
	exists := true
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			exists = false
			cfg = nil
			data = nil
		} else {
			var basicErr *BasicError
			if errors.As(err, &basicErr) {
				return basicErr
			}
			return basicInternal("read client config %s: %v", path, err)
		}
	}

	revision := ""
	if exists {
		revision = contentRevision(data)
	}
	if update.ExpectedRevision == "" {
		if exists {
			return basicConflict("expected_revision is required for an existing configuration")
		}
	} else if update.ExpectedRevision != revision {
		return basicConflict("configuration changed on disk; reload and try again")
	}

	if exists {
		if cfg.IsMulti() {
			return &BasicError{
				Code:    BasicCodeMulti,
				Message: "multi-server configurations are read-only in the basic editor; use the client console",
			}
		}
		if hasUnknownTopLevelKeys(data) {
			return &BasicError{
				Code:    BasicCodeUnknownFields,
				Message: "configuration contains unknown top-level fields; use the client console or SSH",
			}
		}
	} else {
		cfg = &ClientConfig{}
	}
	cfg.ConfigPath = path

	if err := applyBasicUpdate(cfg, update, exists); err != nil {
		return err
	}
	valid, validationError, advanced := validateBasicState(cfg)
	if !valid {
		if advanced {
			return &BasicError{Code: BasicCodeAdvanced, Message: validationError, Advanced: true}
		}
		return basicValidation("%s", validationError)
	}

	if opts.BeforeWrite != nil {
		if err := opts.BeforeWrite(); err != nil {
			var basicErr *BasicError
			if errors.As(err, &basicErr) {
				return basicErr
			}
			return basicInternal("before write: %v", err)
		}
	}
	if err := SaveClient(path, cfg); err != nil && !errors.Is(err, ErrCommitted) {
		return basicInternal("save client config %s: %v", path, err)
	}
	return nil
}

// applyBasicUpdate merges the editable fields into cfg. Advanced fields are
// left untouched unless a server migration clears the client identity.
func applyBasicUpdate(cfg *ClientConfig, update *BasicUpdate, exists bool) error {
	oldServer := cfg.Server
	oldSNI := cfg.TLS.SNI
	oldAuth := cfg.Auth
	oldToken := cfg.Control.Token

	newServer := *update.Server
	newIdentity, err := ServerIdentity(newServer)
	if err != nil {
		return basicValidation("server: %v", err)
	}
	oldIdentity, oldIdentityErr := ServerIdentity(oldServer)
	migrated := oldIdentityErr != nil || oldIdentity != newIdentity

	if migrated && exists {
		if cfg.Credentials.Generation > 1 {
			return basicAdvanced("credentials.generation is greater than 1; use the client console to change the server")
		}
		if serverLocked(cfg) {
			return basicAdvanced("server address cannot be changed while advanced server settings are configured; use the client console or SSH")
		}
		if update.Auth == nil || *update.Auth == "" {
			return basicValidation("auth is required when changing the server")
		}
		if update.ControlToken == nil || *update.ControlToken == "" {
			return basicValidation("control_token is required when changing the server")
		}
		// A migrated server must receive a genuinely new credential pair.
		// Length checks run before the equality checks so a short legacy
		// value is rejected even when it is resubmitted unchanged.
		if len(*update.Auth) < 8 {
			return basicValidation("auth must be at least 8 bytes")
		}
		if len(*update.ControlToken) < 16 {
			return basicValidation("control_token must be at least 16 bytes")
		}
		if *update.Auth == oldAuth {
			return basicValidation("auth must differ from the previous server's auth when changing the server")
		}
		if *update.ControlToken == oldToken {
			return basicValidation("control_token must differ from the previous server's token when changing the server")
		}
	}

	if update.Auth != nil && *update.Auth != "" && *update.Auth != oldAuth {
		if len(*update.Auth) < 8 {
			return basicValidation("auth must be at least 8 bytes")
		}
		if cfg.Credentials.Generation > 1 {
			return basicAdvanced("credentials.generation is greater than 1; use the client console to change auth")
		}
		cfg.Auth = *update.Auth
	}
	if update.ControlToken != nil && *update.ControlToken != "" && *update.ControlToken != oldToken {
		if len(*update.ControlToken) < 16 {
			return basicValidation("control_token must be at least 16 bytes")
		}
		if cfg.Credentials.Generation > 1 {
			return basicAdvanced("credentials.generation is greater than 1; use the client console to change the control token")
		}
		cfg.Control.Token = *update.ControlToken
	}

	if migrated && exists {
		cfg.Client.ID = ""
		cfg.Client.Revision = 0
		if update.TLSSNI == nil || *update.TLSSNI == oldSNI {
			cfg.TLS.SNI = ""
		}
	}
	cfg.Server = newServer

	if update.TLSSNI != nil && !(migrated && exists && *update.TLSSNI == oldSNI) {
		cfg.TLS.SNI = *update.TLSSNI
	}
	if update.TLSInsecure != nil {
		cfg.TLS.Insecure = *update.TLSInsecure
	}
	cfg.Client.Name = *update.ClientName
	cfg.Mappings = append([]Mapping(nil), (*update.Mappings)...)

	cfg.Web.Listen = *update.WebListen
	cfg.Web.Username = *update.WebUsername
	password := ""
	if update.WebPassword != nil {
		password = *update.WebPassword
	}
	if password != "" {
		if cfg.Web.Listen == "" {
			return basicValidation("web_password must be empty when web.listen is disabled")
		}
		if utf8.RuneCountInString(password) < 8 {
			return basicValidation("web_password must be at least 8 characters")
		}
		confirm := ""
		if update.WebPasswordConfirm != nil {
			confirm = *update.WebPasswordConfirm
		}
		if password != confirm {
			return basicValidation("web_password_confirm must match web_password")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return basicInternal("hash web password: %v", err)
		}
		cfg.Web.PasswordHash = string(hash)
	} else if cfg.Web.Listen == "" {
		cfg.Web.PasswordHash = ""
	}
	return nil
}

func basicValidation(format string, args ...any) *BasicError {
	return &BasicError{Code: BasicCodeValidation, Message: fmt.Sprintf(format, args...)}
}

func basicConflict(format string, args ...any) *BasicError {
	return &BasicError{Code: BasicCodeConflict, Message: fmt.Sprintf(format, args...)}
}

func basicAdvanced(format string, args ...any) *BasicError {
	return &BasicError{Code: BasicCodeAdvanced, Message: fmt.Sprintf(format, args...), Advanced: true}
}

func basicInternal(format string, args ...any) *BasicError {
	return &BasicError{Code: BasicCodeInternal, Message: fmt.Sprintf(format, args...)}
}
