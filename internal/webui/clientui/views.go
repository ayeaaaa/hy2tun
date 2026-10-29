package clientui

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"hy2tun/internal/version"
	"hy2tun/internal/view"
	"hy2tun/internal/webui/shared"
)

//go:embed templates/*.html static/*
var assets embed.FS

// MaskedSecret is the fixed placeholder shown instead of a stored secret. It
// deliberately reveals neither the value nor its length.
const MaskedSecret = "••••••••"

// SettingsView is the secret-free settings snapshot used by the settings page
// and GET /api/settings. It never contains a plaintext password or token; the
// current HY2 pair is represented only by a fixed mask.
type SettingsView struct {
	WebUsername           string                 `json:"web_username"`
	WebPasswordConfigured bool                   `json:"web_password_configured"`
	Name                  string                 `json:"name"`
	Credentials           CredentialSettingsView `json:"credentials"`
	Proxy                 ProxyView              `json:"proxy"`
	ReadOnly              []ReadOnlySetting      `json:"read_only"`
}

// ProxyView is the per-server local proxy state shown by the client console.
type ProxyView struct {
	Enabled bool   `json:"enabled"`
	SOCKS5  string `json:"socks5"`
	HTTP    string `json:"http"`
	Error   string `json:"error,omitempty"`
}

// ServerSummary is one row in the client console server list.
type ServerSummary struct {
	Name         string `json:"name"`
	Address      string `json:"address"`
	Online       bool   `json:"online"`
	Mappings     int    `json:"mappings"`
	ProxyEnabled bool   `json:"proxy_enabled"`
}

// CredentialSettingsView describes the client's local credential generation
// and the generation reported by the server's last handshake.
type CredentialSettingsView struct {
	Generation                int64  `json:"generation"`
	ServerGeneration          int64  `json:"server_generation"`
	CurrentAuthMasked         string `json:"current_auth_masked"`
	CurrentControlTokenMasked string `json:"current_control_token_masked"`
	Synced                    bool   `json:"synced"`
}

// ReadOnlySetting is a configuration field that still requires a file edit and
// restart.
type ReadOnlySetting struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type pageBase struct {
	Title    string
	Username string
	Version  string
}

type serversPage struct {
	pageBase
	Servers []ServerSummary
}

type indexPage struct {
	pageBase
	Status         view.ClientStatus
	Mappings       []view.MappingView
	Logs           []view.LogEntry
	Settings       SettingsView
	ActiveMappings int
}

type loginPage struct {
	Username string
	Error    string
}

type statusResponse struct {
	Name                       string `json:"name"`
	Server                     string `json:"server"`
	Version                    string `json:"version"`
	Connected                  bool   `json:"connected"`
	UptimeSeconds              int64  `json:"uptime_seconds"`
	TxBytes                    uint64 `json:"tx_bytes"`
	RxBytes                    uint64 `json:"rx_bytes"`
	ActiveMappings             int    `json:"active_mappings"`
	Mappings                   int    `json:"mappings"`
	CredentialGeneration       int64  `json:"credential_generation"`
	ServerCredentialGeneration int64  `json:"server_credential_generation"`
	CredentialSynced           bool   `json:"credential_synced"`
}

type mappingResponse struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	RemotePort   int    `json:"remote_port"`
	LocalAddr    string `json:"local_addr"`
	Note         string `json:"note"`
	Enabled      bool   `json:"enabled"`
	Status       string `json:"status"`
	Error        string `json:"error,omitempty"`
	AssignedPort int    `json:"assigned_port"`
	TxBytes      uint64 `json:"tx_bytes"`
	RxBytes      uint64 `json:"rx_bytes"`
}

type sseSnapshot struct {
	Status     statusResponse    `json:"status"`
	Mappings   []mappingResponse `json:"mappings"`
	ServerTime time.Time         `json:"server_time"`
}

func (s *Server) parseTemplates() error {
	pages := []struct {
		key   string
		files []string
	}{
		{"index", []string{"templates/layout.html", "templates/index.html"}},
		{"servers", []string{"templates/layout.html", "templates/servers.html"}},
		{"login", []string{"templates/login.html"}},
	}

	s.pages = make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		tmpl, err := shared.ParsePage(assets, page.files...)
		if err != nil {
			return fmt.Errorf("clientui: parse %s template: %w", page.key, err)
		}
		s.pages[page.key] = tmpl
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, status int, page string, data any) {
	tmpl, ok := s.pages[page]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	name := "layout"
	if page == "login" {
		name = "login"
	}

	var buffer bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buffer, name, data); err != nil {
		s.log.Error("render client web page", "page", page, "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buffer.Bytes())
}

func (s *Server) base(title string) pageBase {
	return pageBase{
		Title:    title,
		Username: s.adminUsername(),
		Version:  version.Version,
	}
}

// adminUsername returns the live administrator username, falling back to the
// startup configuration until UpdateAdmin has been called.
func (s *Server) adminUsername() string {
	if username := s.sessions.AdminUsername(); username != "" {
		return username
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.Username
}

func (s *Server) settingsFor(api API) SettingsView {
	settings := api.Settings()
	// Defense in depth: the API is expected to mask already, but a template or
	// JSON response must never carry a plaintext secret even if it does not.
	settings.Credentials.CurrentAuthMasked = maskSecret(settings.Credentials.CurrentAuthMasked)
	settings.Credentials.CurrentControlTokenMasked = maskSecret(settings.Credentials.CurrentControlTokenMasked)
	return settings
}

// maskSecret replaces any non-empty secret with the fixed placeholder. The
// caller never receives the original value.
func maskSecret(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return MaskedSecret
}

func statusResponseFrom(status view.ClientStatus, mappings []view.MappingView) statusResponse {
	return statusResponse{
		Name:                       status.Name,
		Server:                     status.Server,
		Version:                    status.Version,
		Connected:                  status.Connected,
		UptimeSeconds:              int64(status.Uptime / time.Second),
		TxBytes:                    status.TxBytes,
		RxBytes:                    status.RxBytes,
		ActiveMappings:             countActiveMappings(mappings),
		Mappings:                   len(mappings),
		CredentialGeneration:       status.CredentialGeneration,
		ServerCredentialGeneration: status.ServerCredentialGeneration,
		CredentialSynced:           status.CredentialSynced,
	}
}

func countActiveMappings(mappings []view.MappingView) int {
	active := 0
	for _, mapping := range mappings {
		if mapping.Status == "running" {
			active++
		}
	}
	return active
}

func mappingResponseFrom(mapping view.MappingView) mappingResponse {
	return mappingResponse{
		ID:           mapping.ID,
		Type:         mapping.Type,
		RemotePort:   mapping.RemotePort,
		LocalAddr:    mapping.LocalAddr,
		Note:         mapping.Note,
		Enabled:      mapping.Enabled,
		Status:       mapping.Status,
		Error:        mapping.Error,
		AssignedPort: mapping.AssignedPort,
		TxBytes:      mapping.TxBytes,
		RxBytes:      mapping.RxBytes,
	}
}

func mappingResponses(mappings []view.MappingView) []mappingResponse {
	out := make([]mappingResponse, 0, len(mappings))
	for _, mapping := range mappings {
		out = append(out, mappingResponseFrom(mapping))
	}
	return out
}

func wantsHTML(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}
