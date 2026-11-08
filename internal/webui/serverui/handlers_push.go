// Package serverui implements the hy2tund web console: an authenticated,
// server-rendered overview of clients, mappings, traffic, and configuration.
package serverui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/view"
)

type pushRequest struct {
	BaseRevision int64                    `json:"base_revision"`
	Mappings     []config.Mapping         `json:"mappings"`
	Exposures    map[string]view.Exposure `json:"exposures,omitempty"`
}

type pushResponse struct {
	Revision int64 `json:"revision"`
}

type pushResult struct {
	ID           string `json:"id"`
	OK           bool   `json:"ok"`
	AssignedPort int    `json:"assigned_port"`
	Error        string `json:"error"`
}

type pushErrorResponse struct {
	Error   string       `json:"error"`
	Code    string       `json:"code"`
	Results []pushResult `json:"results,omitempty"`
}

// partialApplyResponse tells the browser that the mapping list changed but a
// later exposure/enable transaction failed. Revision lets the open drawer
// refresh and retry against the new state instead of getting a revision
// conflict.
type partialApplyResponse struct {
	Error    string `json:"error"`
	Code     string `json:"code"`
	Revision int64  `json:"revision"`
	Partial  bool   `json:"partial"`
}

func (s *Server) handlePushMappings(w http.ResponseWriter, r *http.Request) {
	clientID := r.PathValue("id")
	if clientID == "" {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_mapping", "客户端 ID 不能为空")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	var request pushRequest
	if err := decoder.Decode(&request); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_body", "请求体不是有效的 JSON")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_body", "请求体包含多余内容")
		return
	}
	if request.BaseRevision < 0 {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_revision", "base_revision 不能为负数")
		return
	}
	if err := validatePushMappings(request.Mappings); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_mapping", err.Error())
		return
	}
	if err := validatePushExposures(request.Mappings, request.Exposures); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_exposure", err.Error())
		return
	}

	if len(request.Exposures) == 0 {
		s.pushMappings(w, clientID, request.BaseRevision, request.Mappings)
		return
	}

	exposureAPI, ok := s.api.(ExposureAPI)
	if !ok {
		writeJSONErrorCode(w, http.StatusNotImplemented, "not_implemented",
			"当前服务端 API 不支持新增映射时设置暴露范围")
		return
	}
	current, ok := s.api.Client(clientID)
	if !ok {
		writeJSONErrorCode(w, http.StatusNotFound, "client_not_found", "客户端不存在")
		return
	}

	// A newly added mapping that must be local and enabled cannot be pushed
	// enabled first: the server would bind its default public listeners before
	// the exposure switch. Push it disabled, persist local, then push the final
	// enabled list. This keeps local a hard boundary from the first bind.
	firstMappings := append([]config.Mapping(nil), request.Mappings...)
	var preLocal []string
	for i, mapping := range firstMappings {
		currentMapping, exists := findClientMapping(current, mapping.ID)
		if mapping.Enabled && request.Exposures[mapping.ID] == view.ExposureLocal &&
			(!exists || currentMapping.Status != "running") {
			firstMappings[i].Enabled = false
			preLocal = append(preLocal, mapping.ID)
		}
	}

	revision := request.BaseRevision
	if len(preLocal) > 0 {
		var err error
		revision, err = s.api.PushMappings(clientID, request.BaseRevision, firstMappings)
		if err != nil {
			s.writePushError(w, err)
			return
		}
		for _, mappingID := range preLocal {
			if _, err := exposureAPI.SetMappingExposure(clientID, mappingID, view.ExposureLocal); err != nil {
				s.writePartialApply(w, revision, "映射已创建为禁用状态，暴露范围设置失败，请刷新后继续", err)
				return
			}
		}
	}

	partialRevision := revision
	revision, err := s.api.PushMappings(clientID, revision, request.Mappings)
	if err != nil {
		if len(preLocal) > 0 {
			s.writePartialApply(w, partialRevision, "映射已创建为禁用状态，启用推送失败，请刷新后继续", err)
		} else {
			s.writePushError(w, err)
		}
		return
	}

	// Apply all remaining exposure decisions after the mapping list is in its
	// final shape. New public mappings are explicit so a stale deleted-ID
	// exposure cannot leak into the recreated mapping.
	for mappingID, exposure := range request.Exposures {
		if containsString(preLocal, mappingID) {
			continue
		}
		if currentMapping, exists := findClientMapping(current, mappingID); exists &&
			exposureOrDefault(currentMapping.Exposure) == exposure {
			continue
		}
		if _, err := exposureAPI.SetMappingExposure(clientID, mappingID, exposure); err != nil {
			s.writePartialApply(w, revision, "映射已更新，但暴露范围设置失败，请刷新后继续", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, pushResponse{Revision: revision})
}

func (s *Server) writePartialApply(w http.ResponseWriter, revision int64, message string, err error) {
	s.log.Warn("mapping push partially applied", "revision", revision, "error", err)
	writeJSON(w, http.StatusConflict, partialApplyResponse{
		Error:    message,
		Code:     "partial_apply",
		Revision: revision,
		Partial:  true,
	})
}

func (s *Server) pushMappings(w http.ResponseWriter, clientID string, baseRevision int64, mappings []config.Mapping) {
	revision, err := s.api.PushMappings(clientID, baseRevision, mappings)
	if err != nil {
		s.writePushError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pushResponse{Revision: revision})
}

func validatePushExposures(mappings []config.Mapping, exposures map[string]view.Exposure) error {
	if len(exposures) == 0 {
		return nil
	}
	known := make(map[string]struct{}, len(mappings))
	for _, mapping := range mappings {
		known[mapping.ID] = struct{}{}
	}
	for mappingID, exposure := range exposures {
		if _, ok := known[mappingID]; !ok {
			return fmt.Errorf("exposures.%s 不对应任何映射", mappingID)
		}
		if !exposure.Valid() {
			return fmt.Errorf("exposures.%s 必须是 public 或 local", mappingID)
		}
	}
	return nil
}

func findClientMapping(client view.ClientView, mappingID string) (view.MappingView, bool) {
	for _, mapping := range client.Mappings {
		if mapping.ID == mappingID {
			return mapping, true
		}
	}
	return view.MappingView{}, false
}

func exposureOrDefault(exposure view.Exposure) view.Exposure {
	if exposure == "" {
		return view.ExposurePublic
	}
	return exposure
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func validatePushMappings(mappings []config.Mapping) error {
	seen := make(map[string]struct{}, len(mappings))
	for i, mapping := range mappings {
		field := fmt.Sprintf("mappings[%d]", i)
		if mapping.ID == "" {
			return fmt.Errorf("%s.id 不能为空", field)
		}
		if _, duplicate := seen[mapping.ID]; duplicate {
			return fmt.Errorf("%s.id %q 重复", field, mapping.ID)
		}
		seen[mapping.ID] = struct{}{}

		if mapping.Type != "tcp" && mapping.Type != "udp" {
			return fmt.Errorf("%s.type 必须是 tcp 或 udp", field)
		}
		if mapping.RemotePort < 0 || mapping.RemotePort > 65535 {
			return fmt.Errorf("%s.remote_port 必须在 0 到 65535 之间", field)
		}
		host, portText, err := net.SplitHostPort(mapping.LocalAddr)
		if err != nil {
			return fmt.Errorf("%s.local_addr 必须是 host:port", field)
		}
		if host == "" {
			return fmt.Errorf("%s.local_addr 必须包含主机", field)
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("%s.local_addr 端口无效", field)
		}
		if len([]rune(mapping.Note)) > 128 {
			return fmt.Errorf("%s.note 不能超过 128 个字符", field)
		}
	}
	return nil
}

func (s *Server) writePushError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, control.ErrConflict):
		writeJSONErrorCode(w, http.StatusConflict, "conflict", "客户端配置已变化，请刷新后重试")
		return
	case errors.Is(err, control.ErrClientOffline):
		writeJSONErrorCode(w, http.StatusConflict, "client_offline", "客户端离线，无法修改配置")
		return
	case errors.Is(err, control.ErrClientNotReady):
		writeJSONErrorCode(w, http.StatusConflict, "client_not_ready", "客户端正在连接，请稍后重试")
		return
	case errors.Is(err, control.ErrPushTimeout):
		writeJSONErrorCode(w, http.StatusGatewayTimeout, "push_timeout", "客户端未在超时时间内确认配置")
		return
	}

	var provider interface{ MappingResults() []control.Result }
	if errors.As(err, &provider) {
		results := provider.MappingResults()
		status := http.StatusConflict
		code := "push_rejected"
		for _, result := range results {
			if result.Error == "" {
				continue
			}
			if code == "push_rejected" {
				code = result.Error
			}
			switch result.Error {
			case string(control.CodeBadMessage), string(control.CodeInvalidTarget):
				status = http.StatusBadRequest
			}
		}

		payload := pushErrorResponse{Error: "映射配置被拒绝", Code: code}
		payload.Results = make([]pushResult, 0, len(results))
		for _, result := range results {
			payload.Results = append(payload.Results, pushResult{
				ID:           result.ID,
				OK:           result.OK,
				AssignedPort: result.AssignedPort,
				Error:        result.Error,
			})
		}
		writeJSON(w, status, payload)
		return
	}

	s.log.Warn("mapping push failed", "error", err)
	writeJSONErrorCode(w, http.StatusInternalServerError, "internal", "推送配置失败")
}
