package clientui

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"hy2tun/internal/config"
)

// ModeAPI is the optional transport-mode mutation supported by a client worker
// or supervisor. Embedded and fake APIs that predate the control remain valid;
// their mode endpoint reports 501 and the page omits the editable controls.
type ModeAPI interface {
	UpdateMode(config.TransportMode) error
}

func (s *Server) handleUpdateMode(w http.ResponseWriter, r *http.Request) {
	api, ok := s.apiFor(r)
	if !ok {
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}
	modeAPI, ok := api.(ModeAPI)
	if !ok {
		writeJSONErrorCode(w, http.StatusNotImplemented, "not_implemented",
			"当前客户端 API 不支持在线切换传输模式")
		return
	}
	var request struct {
		passwordRequest
		Mode config.TransportMode `json:"mode"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	if err := request.Mode.Validate(); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_mode", err.Error())
		return
	}
	if err := modeAPI.UpdateMode(request.Mode); err != nil {
		// The mode itself was validated above; a failure here is a persistence
		// or runtime error and must not be reported as a bad request.
		s.log.Warn("client transport mode update failed", "error", err)
		writeJSONErrorCode(w, http.StatusInternalServerError, "internal", "更新传输模式失败")
		return
	}
	writeJSON(w, http.StatusOK, api.Status().Transport)
}

// writeTransportMetrics emits per-server transport diagnostics after the
// existing aggregate metrics. Only configured worker names are used as label
// values; targets, credentials, tokens and user-provided text are never labels.
func (s *Server) writeTransportMetrics(w io.Writer) {
	summaries := s.transportMetricSummaries()
	_, _ = fmt.Fprint(w, `# HELP hy2tun_client_transport_active Whether this server's actual wire transport is active.
# TYPE hy2tun_client_transport_active gauge
# HELP hy2tun_client_transport_switches_total Successful transport switches for this server.
# TYPE hy2tun_client_transport_switches_total counter
`)
	for _, summary := range summaries {
		hy2, tcp := 0, 0
		switch summary.ActiveTransport {
		case "hy2":
			hy2 = 1
		case "tcp":
			tcp = 1
		}
		server := escapeMetricLabel(summary.Name)
		_, _ = fmt.Fprintf(w, "hy2tun_client_transport_active{server=\"%s\",transport=\"hy2\"} %d\n", server, hy2)
		_, _ = fmt.Fprintf(w, "hy2tun_client_transport_active{server=\"%s\",transport=\"tcp\"} %d\n", server, tcp)
		_, _ = fmt.Fprintf(w, "hy2tun_client_transport_switches_total{server=\"%s\"} %d\n", server, summary.SwitchCount)
	}
}

func (s *Server) transportMetricSummaries() []ServerSummary {
	if s.multi != nil {
		return s.multi.Servers()
	}
	status := s.api.Status()
	return []ServerSummary{{
		Name:           "default",
		Address:        status.Server,
		Online:         status.Connected,
		Mappings:       len(s.api.Mappings()),
		TransportState: status.Transport,
	}}
}

// escapeMetricLabel escapes the characters that Prometheus text exposition
// requires inside a label value.
func escapeMetricLabel(value string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"\n", `\n`,
		`"`, `\"`,
	)
	return replacer.Replace(value)
}
