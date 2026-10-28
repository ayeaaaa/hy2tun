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
)

type pushRequest struct {
	BaseRevision int64            `json:"base_revision"`
	Mappings     []config.Mapping `json:"mappings"`
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

	revision, err := s.api.PushMappings(clientID, request.BaseRevision, request.Mappings)
	if err != nil {
		s.writePushError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pushResponse{Revision: revision})
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
