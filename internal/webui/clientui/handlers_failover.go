package clientui

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"hy2tun/internal/config"
)

// FailoverAPI is the optional per-server failover timing mutation. Embedded
// and fake APIs that predate the control remain valid; their endpoint reports
// 501 and the page omits the editable form.
type FailoverAPI interface {
	UpdateFailover(config.FailoverConfig) error
}

func (s *Server) handleUpdateFailover(w http.ResponseWriter, r *http.Request) {
	api, ok := s.apiFor(r)
	if !ok {
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}
	failoverAPI, ok := api.(FailoverAPI)
	if !ok {
		writeJSONErrorCode(w, http.StatusNotImplemented, "not_implemented",
			"当前客户端 API 不支持在线调整回退时序")
		return
	}
	var request struct {
		passwordRequest
		ConnectTimeout    string `json:"connect_timeout"`
		HeartbeatInterval string `json:"heartbeat_interval"`
		HeartbeatTimeout  string `json:"heartbeat_timeout"`
		ProbeInterval     string `json:"probe_interval"`
		MinTCPHold        string `json:"min_tcp_hold"`
		RecoverySuccesses int    `json:"recovery_successes"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	connectTimeout, err := parseFailoverDuration("connect_timeout", request.ConnectTimeout)
	if err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_failover", err.Error())
		return
	}
	heartbeatInterval, err := parseFailoverDuration("heartbeat_interval", request.HeartbeatInterval)
	if err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_failover", err.Error())
		return
	}
	heartbeatTimeout, err := parseFailoverDuration("heartbeat_timeout", request.HeartbeatTimeout)
	if err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_failover", err.Error())
		return
	}
	probeInterval, err := parseFailoverDuration("probe_interval", request.ProbeInterval)
	if err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_failover", err.Error())
		return
	}
	minTCPHold, err := parseFailoverDuration("min_tcp_hold", request.MinTCPHold)
	if err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_failover", err.Error())
		return
	}
	next := config.FailoverConfig{
		ConnectTimeout:    connectTimeout,
		HeartbeatInterval: heartbeatInterval,
		HeartbeatTimeout:  heartbeatTimeout,
		ProbeInterval:     probeInterval,
		MinTCPHold:        minTCPHold,
		RecoverySuccesses: request.RecoverySuccesses,
	}
	if err := next.Validate("failover"); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_failover", err.Error())
		return
	}
	if err := failoverAPI.UpdateFailover(next); err != nil {
		s.log.Warn("client failover update failed", "error", err)
		writeJSONErrorCode(w, http.StatusInternalServerError, "internal", "更新回退时序失败")
		return
	}
	writeJSON(w, http.StatusOK, api.Settings().Failover)
}

func parseFailoverDuration(name, value string) (config.Duration, error) {
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return config.Duration(parsed), nil
}
