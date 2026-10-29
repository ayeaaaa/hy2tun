package clientui

import (
	"net/http"
	"strings"
)

// apiFor resolves the server selected by the `server` query parameter. Legacy
// single-server consoles have no multi API and always return their only API.
// Multi-server consoles require a `default` server for unscoped legacy API
// calls; they never silently choose the first server.
func (s *Server) apiFor(r *http.Request) (API, bool) {
	if s.multi == nil {
		return s.api, true
	}
	name := strings.TrimSpace(r.URL.Query().Get("server"))
	if name == "" {
		name = "default"
	}
	return s.multi.Server(name)
}

func (s *Server) serverSummaries() []ServerSummary {
	if s.multi != nil {
		return s.multi.Servers()
	}
	status := s.api.Status()
	settings := s.api.Settings()
	return []ServerSummary{{
		Name:         "default",
		Address:      status.Server,
		Online:       status.Connected,
		Mappings:     len(s.api.Mappings()),
		ProxyEnabled: settings.Proxy.Enabled,
	}}
}

func (s *Server) handleServersPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "servers", serversPage{
		pageBase: s.base("服务端"),
		Servers:  s.serverSummaries(),
	})
}

func (s *Server) handleServersJSON(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.serverSummaries())
}
