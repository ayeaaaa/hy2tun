package serverui

import (
	"net/http"

	"hy2tun/internal/config"
)

// ModeAPI is the optional transport-mode mutation supported by a server. APIs
// that predate the control remain valid; their mode endpoint reports 501 and
// the settings page omits the editable controls.
type ModeAPI interface {
	UpdateMode(config.TransportMode) error
}

func (s *Server) handleUpdateMode(w http.ResponseWriter, r *http.Request) {
	modeAPI, ok := s.api.(ModeAPI)
	if !ok {
		writeJSONErrorCode(w, http.StatusNotImplemented, "not_implemented",
			"当前服务端 API 不支持在线切换传输模式")
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
		s.log.Warn("server transport mode update failed", "error", err)
		writeJSONErrorCode(w, http.StatusInternalServerError, "internal", "更新传输模式失败")
		return
	}
	writeJSON(w, http.StatusOK, s.settingsFor())
}
