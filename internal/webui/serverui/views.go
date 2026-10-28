package serverui

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

// ConfigSummary is the read-only server configuration shown on the settings
// page. It deliberately contains no passwords, tokens, or key material.
type ConfigSummary struct {
	ConfigPath             string        `json:"config_path"`
	Listen                 string        `json:"listen"`
	ListenHost             string        `json:"listen_host"`
	WebListen              string        `json:"web_listen"`
	WebUsername            string        `json:"web_username"`
	WebTLS                 bool          `json:"web_tls"`
	TLS                    bool          `json:"tls"`
	StateFile              string        `json:"state_file"`
	PortsAllow             []string      `json:"ports_allow"`
	PortsDeny              []string      `json:"ports_deny"`
	PortsAutoRange         string        `json:"ports_auto_range"`
	OfflineReserve         time.Duration `json:"-"`
	OfflineReserveSeconds  int64         `json:"offline_reserve_seconds"`
	TrustProxy             bool          `json:"trust_proxy"`
	LogLevel               string        `json:"log_level"`
	Version                string        `json:"version"`
	ControlTokenConfigured bool          `json:"control_token_configured"`
	WebPasswordConfigured  bool          `json:"web_password_configured"`
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
	Summary  ConfigSummary
	Overview view.Overview
}

type loginPage struct {
	Username string
	Error    string
}

type clientDetailData struct {
	Client      view.ClientView
	ActiveCount int
	TxBytes     uint64
	RxBytes     uint64
}

type mappingRow struct {
	ClientID   string
	ClientName string
	Online     bool
	Mapping    view.MappingView
}

type mappingEntry struct {
	ClientID   string           `json:"client_id"`
	ClientName string           `json:"client_name"`
	Online     bool             `json:"online"`
	Mapping    view.MappingView `json:"mapping"`
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
		Username: s.cfg.Username,
		Version:  version.Version,
	}
}

func (s *Server) configSummary() ConfigSummary {
	if provider, ok := s.api.(interface{ ConfigSummary() ConfigSummary }); ok {
		return provider.ConfigSummary()
	}
	return ConfigSummary{
		WebListen:             s.cfg.Listen,
		WebUsername:           s.cfg.Username,
		WebTLS:                s.cfg.TLS.Cert != "",
		WebPasswordConfigured: s.cfg.PasswordHash != "",
		Version:               version.Version,
	}
}

func clientDetailDataFrom(client view.ClientView) clientDetailData {
	active := 0
	var txBytes, rxBytes uint64
	for _, mapping := range client.Mappings {
		if mapping.Status == "running" {
			active++
		}
		txBytes += mapping.TxBytes
		rxBytes += mapping.RxBytes
	}
	return clientDetailData{
		Client:      client,
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
				Mapping:    mapping,
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

func wantsHTML(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html")
}
