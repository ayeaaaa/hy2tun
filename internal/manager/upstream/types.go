// Package upstream is the manager's HTTP adapter for an existing hy2tund
// server. It uses only the server's public Web API and never imports the
// server runtime or its templates.
package upstream

import (
	"time"

	"hy2tun/internal/config"
	"hy2tun/internal/view"
)

// ServerRecord is the subset of the manager inventory record needed to open an
// upstream session. The encrypted password and the remaining inventory fields
// are deliberately not part of this type.
type ServerRecord struct {
	ID               string
	Name             string
	BaseURL          string
	UpstreamUsername string
	TLSSkipVerify    bool
}

// Overview is the secret-free snapshot returned by GET /api/overview.
type Overview struct {
	OnlineClients  int
	ActiveMappings int
	TxBytes        uint64
	RxBytes        uint64
	Listen         string
	Uptime         time.Duration
}

// ClientView is one client as returned by GET /api/clients and
// GET /api/clients/{id}. hy2tund encodes view.ClientView without JSON tags, so
// the exported field names are the stable wire names.
type ClientView struct {
	ID                   string
	Name                 string
	Version              string
	Revision             int64
	Online               bool
	LastSeen             time.Time
	Mappings             []MappingView
	Transport            string
	CredentialGeneration int64
	CredentialSynced     bool
	CredentialCapable    bool
}

// MappingView is one mapping with its runtime state. config.Mapping supplies
// the persisted fields and their existing JSON names; the remaining fields
// match hy2tund's view.MappingView wire names.
type MappingView struct {
	config.Mapping
	Status              string
	Error               string
	AssignedPort        int
	Exposure            view.Exposure
	UnavailableFamilies []string
	TxBytes             uint64
	RxBytes             uint64
}

// Settings is the secret-free settings snapshot returned by GET /api/settings.
// Secrets are represented only by the server's fixed mask.
type Settings struct {
	WebUsername           string               `json:"web_username"`
	WebPasswordConfigured bool                 `json:"web_password_configured"`
	AuthType              string               `json:"auth_type"`
	TrustProxy            bool                 `json:"trust_proxy"`
	Mode                  config.TransportMode `json:"mode"`
	Ports                 PortSettings         `json:"ports"`
	Credentials           CredentialSettings   `json:"credentials"`
	Clients               []ClientCredential   `json:"clients"`
	ReadOnly              []ReadOnlySetting    `json:"read_only"`
}

// PortSettings is the hot-reloadable port policy.
type PortSettings struct {
	Allow                 []string      `json:"allow"`
	Deny                  []string      `json:"deny"`
	AutoRange             string        `json:"auto_range"`
	OfflineReserveSeconds int64         `json:"offline_reserve_seconds"`
	OfflineReserve        time.Duration `json:"-"`
}

// CredentialSettings describes current and previous credential generations.
type CredentialSettings struct {
	Generation                 int64  `json:"generation"`
	PreviousGeneration         int64  `json:"previous_generation"`
	CurrentAuthMasked          string `json:"current_auth_masked"`
	CurrentControlTokenMasked  string `json:"current_control_token_masked"`
	PreviousAuthMasked         string `json:"previous_auth_masked"`
	PreviousControlTokenMasked string `json:"previous_control_token_masked"`
	Rotatable                  bool   `json:"rotatable"`
}

// ClientCredential is one row of the credential sync table.
type ClientCredential struct {
	ClientID   string    `json:"client_id"`
	Name       string    `json:"name"`
	Online     bool      `json:"online"`
	Generation int64     `json:"generation"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	LastSeen   time.Time `json:"last_seen"`
}

// ReadOnlySetting is a configuration value that still requires a file edit and
// a server restart.
type ReadOnlySetting struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// ConfigSummary is the read-only configuration and version snapshot returned
// by GET /api/config/summary. It contains no secrets or key material.
type ConfigSummary struct {
	ConfigPath             string               `json:"config_path"`
	Listen                 string               `json:"listen"`
	ListenHost             string               `json:"listen_host"`
	WebListen              string               `json:"web_listen"`
	WebUsername            string               `json:"web_username"`
	WebTLS                 bool                 `json:"web_tls"`
	TLS                    bool                 `json:"tls"`
	StateFile              string               `json:"state_file"`
	PortsAllow             []string             `json:"ports_allow"`
	PortsDeny              []string             `json:"ports_deny"`
	PortsAutoRange         string               `json:"ports_auto_range"`
	OfflineReserveSeconds  int64                `json:"offline_reserve_seconds"`
	OfflineReserve         time.Duration        `json:"-"`
	TrustProxy             bool                 `json:"trust_proxy"`
	LogLevel               string               `json:"log_level"`
	Mode                   config.TransportMode `json:"mode"`
	Version                string               `json:"version"`
	ControlTokenConfigured bool                 `json:"control_token_configured"`
	WebPasswordConfigured  bool                 `json:"web_password_configured"`
}

// RotateResult is the response to PUT /api/settings/credentials.
type RotateResult struct {
	Generation int64              `json:"generation"`
	Results    []CredentialResult `json:"results"`
}

// CredentialResult is one client's credential-sync outcome. hy2tund encodes
// view.CredentialResult without JSON tags.
type CredentialResult struct {
	ClientID   string
	Name       string
	Generation int64
	Status     string
	Error      string
}
