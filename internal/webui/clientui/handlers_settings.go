// Package clientui implements the hy2tun client web console: an authenticated,
// server-rendered view of the local connection, mappings, logs, and read-only
// settings.
package clientui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/webui/shared"
)

type passwordRequest struct {
	Password        string `json:"password"`
	CurrentPassword string `json:"current_password"`
}

func (p passwordRequest) secret() string {
	if p.CurrentPassword != "" {
		return p.CurrentPassword
	}
	return p.Password
}

func decodeSettingsBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_body", "请求体不是有效的 JSON")
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_body", "请求体包含多余内容")
		return false
	}
	return true
}

// confirmCurrentPassword implements the re-authentication required by every
// sensitive settings mutation. Failed attempts are rate-limited per client IP.
// The submitted value is never logged.
func (s *Server) confirmCurrentPassword(w http.ResponseWriter, r *http.Request, password string) bool {
	ip := shared.ClientIPWithProxy(r, s.cfg.TrustProxy)
	if !s.settingsLimiter.Allow(ip) {
		w.Header().Set("Retry-After", "60")
		writeJSONErrorCode(w, http.StatusTooManyRequests, "rate_limited", "尝试次数过多，请稍后再试")
		return false
	}
	if s.sessions.VerifyPassword(password) {
		s.settingsLimiter.Success(ip)
		return true
	}
	s.settingsLimiter.Failure(ip)
	writeJSONErrorCode(w, http.StatusForbidden, "invalid_password", "当前密码不正确")
	return false
}

func (s *Server) writeSettingsMutationError(w http.ResponseWriter, err error, code, message string) {
	status := http.StatusBadRequest
	if strings.Contains(err.Error(), "save client config") || strings.Contains(err.Error(), "write config") {
		status = http.StatusInternalServerError
		code = "internal"
		message = "写入配置文件失败"
	}
	if message == "" {
		message = err.Error()
	}
	s.log.Warn("client settings mutation failed", "code", code, "error", err)
	writeJSONErrorCode(w, status, code, message)
}

func (s *Server) handleUpdateWebSettings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		passwordRequest
		Username        string `json:"username"`
		NewPassword     string `json:"new_password"`
		ConfirmPassword string `json:"confirm_password"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	// Serialize the complete verify -> hash -> persist -> session-update
	// sequence so two requests cannot both verify the old password and race
	// their session updates.
	s.webAdminMu.Lock()
	defer s.webAdminMu.Unlock()
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	username := strings.TrimSpace(request.Username)
	if username == "" {
		writeJSONErrorCode(w, http.StatusBadRequest, "username_required", "用户名不能为空")
		return
	}
	if len([]rune(request.NewPassword)) < 8 {
		writeJSONErrorCode(w, http.StatusBadRequest, "weak_password", "新密码至少需要 8 个字符")
		return
	}
	if len([]byte(request.NewPassword)) > 72 {
		writeJSONErrorCode(w, http.StatusBadRequest, "weak_password", "新密码不能超过 72 字节")
		return
	}
	if request.ConfirmPassword != "" && request.ConfirmPassword != request.NewPassword {
		writeJSONErrorCode(w, http.StatusBadRequest, "password_mismatch", "两次输入的新密码不一致")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(request.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		s.log.Error("hash client web password", "error", err)
		writeJSONErrorCode(w, http.StatusInternalServerError, "internal", "无法保存新密码")
		return
	}
	if err := s.api.UpdateWebConfig(username, string(hash)); err != nil {
		s.writeSettingsMutationError(w, err, "invalid_web_config", "更新 Web 管理员失败")
		return
	}
	// UpdateWebConfig persists the config only. The live login path must be
	// updated here, and every existing session is revoked because the
	// administrator identity changed.
	s.sessions.UpdateAdmin(username, string(hash))
	s.sessions.RevokeAll()
	s.login.SetAdmin(username, string(hash))
	s.mu.Lock()
	s.cfg.Username = username
	s.cfg.PasswordHash = string(hash)
	s.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpdateName(w http.ResponseWriter, r *http.Request) {
	api, ok := s.apiFor(r)
	if !ok {
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}
	var request struct {
		passwordRequest
		Name string `json:"name"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		writeJSONErrorCode(w, http.StatusBadRequest, "name_required", "客户端名称不能为空")
		return
	}
	if err := api.UpdateName(name); err != nil {
		s.writeSettingsMutationError(w, err, "invalid_name", "更新客户端名称失败")
		return
	}
	writeJSON(w, http.StatusOK, s.settingsFor(api))
}

func (s *Server) handleRotateCredentials(w http.ResponseWriter, r *http.Request) {
	api, ok := s.apiFor(r)
	if !ok {
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}
	var request struct {
		passwordRequest
		Generation   int64  `json:"generation"`
		Auth         string `json:"auth"`
		ControlToken string `json:"control_token"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	if request.Generation < 1 {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_generation", "代际必须是正整数")
		return
	}
	if request.Auth == "" {
		writeJSONErrorCode(w, http.StatusBadRequest, "auth_required", "HY2 密码不能为空")
		return
	}
	if len(request.Auth) < 8 {
		writeJSONErrorCode(w, http.StatusBadRequest, "weak_auth", "HY2 密码至少需要 8 个字节")
		return
	}
	if request.ControlToken == "" {
		writeJSONErrorCode(w, http.StatusBadRequest, "control_token_required", "control token 不能为空")
		return
	}
	current := api.Settings().Credentials.Generation
	if current < 1 {
		current = 1
	}
	if request.Generation <= current {
		writeJSONErrorCode(w, http.StatusBadRequest, "generation_must_advance",
			fmt.Sprintf("新代际必须大于当前代际 %d", current))
		return
	}
	if err := api.RotateLocalCredentials(request.Generation, request.Auth, request.ControlToken); err != nil {
		s.writeSettingsMutationError(w, err, "invalid_credentials", err.Error())
		return
	}
	// The client writes the config and forces a reconnect before returning.
	// The new generation is only confirmed after the next hello_ack, so the
	// UI polls /api/status rather than waiting here.
	writeJSON(w, http.StatusAccepted, map[string]any{
		"generation": request.Generation,
		"status":     "pending",
	})
}

type revealedCredential struct {
	Generation   int64  `json:"generation"`
	Auth         string `json:"auth"`
	ControlToken string `json:"control_token"`
}

type revealCredentialsResponse struct {
	Current *revealedCredential `json:"current"`
}

func (s *Server) handleRevealCredentials(w http.ResponseWriter, r *http.Request) {
	api, ok := s.apiFor(r)
	if !ok {
		http.Error(w, "server not found", http.StatusNotFound)
		return
	}
	var request passwordRequest
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	set := api.CredentialsSnapshot()
	if set == nil {
		writeJSONErrorCode(w, http.StatusInternalServerError, "internal", "凭据不可用")
		return
	}
	payload := revealCredentialsResponse{
		Current: &revealedCredential{
			Generation:   set.Generation,
			Auth:         set.Auth,
			ControlToken: set.ControlToken,
		},
	}
	// writeJSON sets this too; set it before the body is written so even a
	// later encoder error cannot downgrade the cache policy.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, payload)
}
