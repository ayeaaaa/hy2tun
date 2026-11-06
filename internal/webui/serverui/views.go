package serverui

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"hy2tun/internal/config"
	"hy2tun/internal/version"
	"hy2tun/internal/view"
	"hy2tun/internal/webui/shared"
)

//go:embed templates/*.html static/*
var assets embed.FS

// ConfigSummary is the read-only server configuration shown on the settings
// page. It deliberately contains no passwords, tokens, or key material.
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
	OfflineReserve         time.Duration        `json:"-"`
	OfflineReserveSeconds  int64                `json:"offline_reserve_seconds"`
	TrustProxy             bool                 `json:"trust_proxy"`
	LogLevel               string               `json:"log_level"`
	Mode                   config.TransportMode `json:"mode"`
	Version                string               `json:"version"`
	ControlTokenConfigured bool                 `json:"control_token_configured"`
	WebPasswordConfigured  bool                 `json:"web_password_configured"`
}

type pageBase struct {
	Title    string
	Active   string
	Username string
	Version  string
}

type overviewPage struct {
	pageBase
	Overview view.Overview
	Clients  []view.ClientView
	Summary  ConfigSummary
}

type clientsPage struct {
	pageBase
	Clients     []view.ClientView
	Selected    *view.ClientView
	Detail      *clientDetailData
	OnlineCount int
	Notice      string
}

type mappingsPage struct {
	pageBase
	Rows []mappingRow
}

type settingsPage struct {
	pageBase
	Settings      SettingsView
	ModeAvailable bool
}

// MaskedSecret is the fixed placeholder shown instead of a stored secret.
// It deliberately reveals neither the value nor its length.
const MaskedSecret = "********"

// SettingsView is the secret-free settings snapshot used by the settings page
// and GET /api/settings. It never contains a plaintext password or token.
type SettingsView struct {
	WebUsername           string                 `json:"web_username"`
	WebPasswordConfigured bool                   `json:"web_password_configured"`
	AuthType              string                 `json:"auth_type"`
	TrustProxy            bool                   `json:"trust_proxy"`
	Mode                  config.TransportMode   `json:"mode"`
	Ports                 PortSettingsView       `json:"ports"`
	Credentials           CredentialSettingsView `json:"credentials"`
	Clients               []ClientCredentialView `json:"clients"`
	ReadOnly              []ReadOnlySetting      `json:"read_only"`
}

// PortSettingsView is the hot-reloadable port policy.
type PortSettingsView struct {
	Allow                 []string      `json:"allow"`
	Deny                  []string      `json:"deny"`
	AutoRange             string        `json:"auto_range"`
	OfflineReserveSeconds int64         `json:"offline_reserve_seconds"`
	OfflineReserve        time.Duration `json:"-"`
}

// CredentialSettingsView describes the current and previous credential
// generations. Secrets are represented only by a fixed mask.
type CredentialSettingsView struct {
	Generation                 int64  `json:"generation"`
	PreviousGeneration         int64  `json:"previous_generation"`
	CurrentAuthMasked          string `json:"current_auth_masked"`
	CurrentControlTokenMasked  string `json:"current_control_token_masked"`
	PreviousAuthMasked         string `json:"previous_auth_masked"`
	PreviousControlTokenMasked string `json:"previous_control_token_masked"`
	Rotatable                  bool   `json:"rotatable"`
}

// ClientCredentialView is one row of the credential sync table. Offline
// clients are included with their last known generation and last-seen time.
type ClientCredentialView struct {
	ClientID   string    `json:"client_id"`
	Name       string    `json:"name"`
	Online     bool      `json:"online"`
	Generation int64     `json:"generation"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	LastSeen   time.Time `json:"last_seen"`
}

// ReadOnlySetting is a configuration field that still requires a file edit
// and restart.
type ReadOnlySetting struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type loginPage struct {
	Username string
	Error    string
	Version  string
}

func newLoginPage(username, message string) loginPage {
	return loginPage{Username: username, Error: message, Version: version.Version}
}

type clientDetailData struct {
	Client      view.ClientView
	Mappings    []mappingDisplay
	ActiveCount int
	TxBytes     uint64
	RxBytes     uint64
}

// mappingDisplay adds the normalized exposure labels used by the server
// templates. The raw MappingView is embedded so existing template fields keep
// working. Exposure is an operator control on the server console.
type mappingDisplay struct {
	view.MappingView
	ExposureValue     string
	ExposureLabel     string
	ExposureNextLabel string
	UnavailableLabel  string
}

func newMappingDisplay(mapping view.MappingView) mappingDisplay {
	value, label, next := exposureDisplay(mapping.Exposure)
	return mappingDisplay{
		MappingView:       mapping,
		ExposureValue:     value,
		ExposureLabel:     label,
		ExposureNextLabel: next,
		UnavailableLabel:  unavailableFamiliesLabel(mapping.UnavailableFamilies),
	}
}

// exposureDisplay treats the empty legacy value as public. A non-empty
// unknown value is shown as unknown (—) instead of guessing public, matching
// the fail-closed server behavior for corrupted state.
func exposureDisplay(exposure view.Exposure) (value, label, next string) {
	switch exposure {
	case view.ExposureLocal:
		return string(view.ExposureLocal), "仅本机", "公网"
	case view.ExposurePublic, "":
		return string(view.ExposurePublic), "公网", "仅本机"
	default:
		return "", "—", "仅本机"
	}
}

// unavailableFamiliesLabel renders a partial-family bind warning such as
// "IPv6 不可用" or "IPv4/IPv6 不可用". Labels are canonicalized so input order
// cannot produce "IPv6/IPv4".
func unavailableFamiliesLabel(families []string) string {
	hasIPv4, hasIPv6 := false, false
	others := make([]string, 0, len(families))
	seen := make(map[string]bool, len(families))
	for _, family := range families {
		var label string
		switch strings.ToLower(strings.TrimSpace(family)) {
		case "ipv4":
			hasIPv4 = true
			continue
		case "ipv6":
			hasIPv6 = true
			continue
		default:
			label = strings.TrimSpace(family)
		}
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		others = append(others, label)
	}
	labels := make([]string, 0, 2+len(others))
	if hasIPv4 {
		labels = append(labels, "IPv4")
	}
	if hasIPv6 {
		labels = append(labels, "IPv6")
	}
	labels = append(labels, others...)
	if len(labels) == 0 {
		return ""
	}
	return strings.Join(labels, "/") + " 不可用"
}

type mappingRow struct {
	ClientID   string
	ClientName string
	Online     bool
	Mapping    mappingDisplay
}

type mappingEntry struct {
	ClientID   string           `json:"client_id"`
	ClientName string           `json:"client_name"`
	Online     bool             `json:"online"`
	Mapping    view.MappingView `json:"mapping"`
}

// exposureConflictResponse describes the state that remains active after a
// failed live switch. Closed means the rollback bind also failed and the
// mapping currently has no listener.
type exposureConflictResponse struct {
	Error               string        `json:"error"`
	Code                string        `json:"code"`
	ActiveExposure      view.Exposure `json:"active_exposure"`
	UnavailableFamilies []string      `json:"unavailable_families"`
	Closed              bool          `json:"closed"`
}

type overviewResponse struct {
	OnlineClients  int    `json:"online_clients"`
	ActiveMappings int    `json:"active_mappings"`
	TxBytes        uint64 `json:"tx_bytes"`
	RxBytes        uint64 `json:"rx_bytes"`
	Listen         string `json:"listen"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
}

type clientSnapshot struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Online         bool      `json:"online"`
	Revision       int64     `json:"revision"`
	Mappings       int       `json:"mappings"`
	ActiveMappings int       `json:"active_mappings"`
	TxBytes        uint64    `json:"tx_bytes"`
	RxBytes        uint64    `json:"rx_bytes"`
	LastSeen       time.Time `json:"last_seen"`
}

type sseSnapshot struct {
	Overview   overviewResponse `json:"overview"`
	Clients    []clientSnapshot `json:"clients"`
	ServerTime time.Time        `json:"server_time"`
}

func (s *Server) parseTemplates() error {
	pages := []struct {
		key   string
		files []string
	}{
		{"overview", []string{"templates/layout.html", "templates/overview.html"}},
		{"clients", []string{"templates/layout.html", "templates/clients.html", "templates/client_detail.html"}},
		{"mappings", []string{"templates/layout.html", "templates/mappings.html"}},
		{"settings", []string{"templates/layout.html", "templates/settings.html"}},
		{"login", []string{"templates/login.html"}},
		{"client_detail", []string{"templates/client_detail.html"}},
	}

	s.pages = make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		tmpl, err := shared.ParsePage(assets, page.files...)
		if err != nil {
			return fmt.Errorf("serverui: parse %s template: %w", page.key, err)
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
	switch page {
	case "login":
		name = "login"
	case "client_detail":
		name = "client-detail"
	}

	var buffer bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buffer, name, data); err != nil {
		s.log.Error("render web page", "page", page, "error", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buffer.Bytes())
}

func (s *Server) base(title, active string) pageBase {
	return pageBase{
		Title:    title,
		Active:   active,
		Username: s.sessions.AdminUsername(),
		Version:  version.Version,
	}
}

func (s *Server) configSummary() ConfigSummary {
	if provider, ok := s.api.(interface{ ConfigSummary() ConfigSummary }); ok {
		return provider.ConfigSummary()
	}
	s.adminMu.RLock()
	username := s.cfg.Username
	passwordHash := s.cfg.PasswordHash
	s.adminMu.RUnlock()
	return ConfigSummary{
		WebListen:             s.cfg.Listen,
		WebUsername:           username,
		WebTLS:                s.cfg.TLS.Cert != "",
		WebPasswordConfigured: passwordHash != "",
		Version:               version.Version,
	}
}

func clientDetailDataFrom(client view.ClientView) clientDetailData {
	active := 0
	var txBytes, rxBytes uint64
	mappings := make([]mappingDisplay, 0, len(client.Mappings))
	for _, mapping := range client.Mappings {
		if mapping.Status == "running" {
			active++
		}
		txBytes += mapping.TxBytes
		rxBytes += mapping.RxBytes
		mappings = append(mappings, newMappingDisplay(mapping))
	}
	return clientDetailData{
		Client:      client,
		Mappings:    mappings,
		ActiveCount: active,
		TxBytes:     txBytes,
		RxBytes:     rxBytes,
	}
}

func mappingRows(clients []view.ClientView) []mappingRow {
	rows := make([]mappingRow, 0)
	for _, client := range clients {
		for _, mapping := range client.Mappings {
			rows = append(rows, mappingRow{
				ClientID:   client.ID,
				ClientName: client.Name,
				Online:     client.Online,
				Mapping:    newMappingDisplay(mapping),
			})
		}
	}
	return rows
}

func mappingEntries(clients []view.ClientView) []mappingEntry {
	entries := make([]mappingEntry, 0)
	for _, client := range clients {
		for _, mapping := range client.Mappings {
			entries = append(entries, mappingEntry{
				ClientID:   client.ID,
				ClientName: client.Name,
				Online:     client.Online,
				Mapping:    mapping,
			})
		}
	}
	return entries
}

func overviewResponseFrom(overview view.Overview) overviewResponse {
	return overviewResponse{
		OnlineClients:  overview.OnlineClients,
		ActiveMappings: overview.ActiveMappings,
		TxBytes:        overview.TxBytes,
		RxBytes:        overview.RxBytes,
		Listen:         overview.Listen,
		UptimeSeconds:  int64(overview.Uptime / time.Second),
	}
}

func (s *Server) snapshot() sseSnapshot {
	clients := s.api.Clients()
	items := make([]clientSnapshot, 0, len(clients))
	for _, client := range clients {
		active := 0
		for _, mapping := range client.Mappings {
			if mapping.Status == "running" {
				active++
			}
		}
		items = append(items, clientSnapshot{
			ID:             client.ID,
			Name:           client.Name,
			Online:         client.Online,
			Revision:       client.Revision,
			Mappings:       len(client.Mappings),
			ActiveMappings: active,
			TxBytes:        sumMappingBytes(client.Mappings, func(m view.MappingView) uint64 { return m.TxBytes }),
			RxBytes:        sumMappingBytes(client.Mappings, func(m view.MappingView) uint64 { return m.RxBytes }),
			LastSeen:       client.LastSeen,
		})
	}
	return sseSnapshot{
		Overview:   overviewResponseFrom(s.api.Overview()),
		Clients:    items,
		ServerTime: time.Now(),
	}
}

func sumMappingBytes(mappings []view.MappingView, value func(view.MappingView) uint64) uint64 {
	var total uint64
	for _, mapping := range mappings {
		total += value(mapping)
	}
	return total
}

// settingsFor applies defense-in-depth masking to the secret-free settings
// snapshot. The API is expected to mask already, but a custom embedded API must
// not be able to leak plaintext credentials through the console.
func (s *Server) settingsFor() SettingsView {
	settings := s.api.Settings()
	settings.Credentials.CurrentAuthMasked = maskSettingsSecret(settings.Credentials.CurrentAuthMasked)
	settings.Credentials.CurrentControlTokenMasked = maskSettingsSecret(settings.Credentials.CurrentControlTokenMasked)
	settings.Credentials.PreviousAuthMasked = maskSettingsSecret(settings.Credentials.PreviousAuthMasked)
	settings.Credentials.PreviousControlTokenMasked = maskSettingsSecret(settings.Credentials.PreviousControlTokenMasked)
	return settings
}

func maskSettingsSecret(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return MaskedSecret
}

func wantsHTML(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}
