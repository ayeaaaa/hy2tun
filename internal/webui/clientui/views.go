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

// Settings is the read-only client connection summary shown on the settings
// tab. ControlToken is always masked before it reaches a template or JSON
// response.
type Settings struct {
	Server       string `json:"server"`
	TLS          bool   `json:"tls"`
	TLSInsecure  bool   `json:"tls_insecure"`
	TLSSNI       string `json:"tls_sni"`
	Obfs         string `json:"obfs"`
	ControlToken string `json:"control_token"`
	ConfigPath   string `json:"config_path"`
}

// SettingsProvider is optionally implemented by the client API to expose the
// read-only connection settings. It is deliberately separate from API so a
// minimal test fake only needs the six core methods.
type SettingsProvider interface {
	Settings() Settings
}

type pageBase struct {
	Title    string
	Username string
	Version  string
}

type indexPage struct {
	pageBase
	Status         view.ClientStatus
	Mappings       []view.MappingView
	Logs           []view.LogEntry
	Settings       Settings
	ActiveMappings int
}

type loginPage struct {
	Username string
	Error    string
}

type statusResponse struct {
	Name           string `json:"name"`
	Server         string `json:"server"`
	Version        string `json:"version"`
	Connected      bool   `json:"connected"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
	TxBytes        uint64 `json:"tx_bytes"`
	RxBytes        uint64 `json:"rx_bytes"`
	ActiveMappings int    `json:"active_mappings"`
	Mappings       int    `json:"mappings"`
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
		Username: s.cfg.Username,
		Version:  version.Version,
	}
}

func (s *Server) settingsFor(status view.ClientStatus) Settings {
	settings := Settings{}
	if provider, ok := s.api.(SettingsProvider); ok {
		settings = provider.Settings()
	}
	if settings.Server == "" {
		settings.Server = status.Server
	}
	settings.ControlToken = maskControlToken(settings.ControlToken)
	return settings
}

// maskControlToken replaces any configured token with a fixed mask. The caller
// never receives the original value, even if a SettingsProvider returns one by
// mistake.
func maskControlToken(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return "••••••••"
}

func statusResponseFrom(status view.ClientStatus, mappings []view.MappingView) statusResponse {
	return statusResponse{
		Name:           status.Name,
		Server:         status.Server,
		Version:        status.Version,
		Connected:      status.Connected,
		UptimeSeconds:  int64(status.Uptime / time.Second),
		TxBytes:        status.TxBytes,
		RxBytes:        status.RxBytes,
		ActiveMappings: countActiveMappings(mappings),
		Mappings:       len(mappings),
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
