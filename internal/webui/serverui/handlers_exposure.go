// Package serverui implements the hy2tund web console: an authenticated,
// server-rendered overview of clients, mappings, traffic, and configuration.
package serverui

import (
	"errors"
	"net/http"

	"hy2tun/internal/control"
	"hy2tun/internal/view"
)

// ExposureAPI is the optional mapping-exposure mutation supported by a server.
// APIs that predate the control remain valid; their exposure endpoint reports
// 501 and the Web UI can omit the control.
type ExposureAPI interface {
	SetMappingExposure(clientID, mappingID string, exposure view.Exposure) (view.MappingView, error)
}

// Marker interfaces let the handler classify the server's typed errors without
// importing internal/server (which imports this package).
type (
	mappingNotFoundMarker interface {
		MappingNotFound() bool
	}
	invalidExposureMarker interface {
		InvalidExposure() bool
	}
	exposureConflictMarker interface {
		ExposureConflict() bool
		ActiveExposure() view.Exposure
		ActiveUnavailableFamilies() []string
		MappingClosed() bool
	}
)

// handleMappingExposure changes one mapping's public/local exposure. Exposure
// changes are ordinary authenticated mutations: they require a session and the
// CSRF header, but deliberately not the current Web password.
func (s *Server) handleMappingExposure(w http.ResponseWriter, r *http.Request) {
	exposureAPI, ok := s.api.(ExposureAPI)
	if !ok {
		writeJSONErrorCode(w, http.StatusNotImplemented, "not_implemented",
			"当前服务端 API 不支持修改映射暴露范围")
		return
	}

	var request struct {
		Exposure view.Exposure `json:"exposure"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !request.Exposure.Valid() {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_exposure",
			"暴露范围必须是 public 或 local")
		return
	}

	mapping, err := exposureAPI.SetMappingExposure(
		r.PathValue("id"), r.PathValue("mappingID"), request.Exposure)
	if err != nil {
		s.writeExposureError(w, err)
		return
	}
	// Return the raw view.MappingView so the PUT success body has exactly the
	// same field names as the existing GET mapping read models.
	writeJSON(w, http.StatusOK, mapping)
}

// writeExposureError maps the server's typed failures onto the stable Web API
// contract. Bind/rollback conflicts and persistence failures both carry the
// active exposure/listener state, so both are reported as 409
// exposure_conflict; 500 is reserved for errors without that known state.
func (s *Server) writeExposureError(w http.ResponseWriter, err error) {
	var mappingNotFound mappingNotFoundMarker
	if errors.As(err, &mappingNotFound) && mappingNotFound.MappingNotFound() {
		writeJSONErrorCode(w, http.StatusNotFound, "mapping_not_found", "映射不存在")
		return
	}

	var invalidExposure invalidExposureMarker
	if errors.As(err, &invalidExposure) && invalidExposure.InvalidExposure() {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_exposure",
			"暴露范围必须是 public 或 local")
		return
	}

	if errors.Is(err, control.ErrClientNotFound) {
		writeJSONErrorCode(w, http.StatusNotFound, "client_not_found", "客户端不存在")
		return
	}

	var conflict exposureConflictMarker
	if errors.As(err, &conflict) && conflict.ExposureConflict() {
		families := conflict.ActiveUnavailableFamilies()
		if families == nil {
			families = []string{}
		}
		closed := conflict.MappingClosed()
		message := "映射暴露范围切换失败，已保留当前状态"
		if closed {
			message = "映射暴露范围切换失败，映射已关闭"
		}
		writeJSON(w, http.StatusConflict, exposureConflictResponse{
			Error:               message,
			Code:                "exposure_conflict",
			ActiveExposure:      conflict.ActiveExposure(),
			UnavailableFamilies: families,
			Closed:              closed,
		})
		return
	}

	if errors.Is(err, control.ErrClientOffline) {
		writeJSONErrorCode(w, http.StatusConflict, "client_offline",
			"客户端离线，无法修改映射暴露范围")
		return
	}

	s.log.Warn("mapping exposure update failed", "error", err)
	writeJSONErrorCode(w, http.StatusInternalServerError, "internal",
		"更新映射暴露范围失败")
}
