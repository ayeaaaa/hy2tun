// Package serverui implements the hy2tund web console: an authenticated,
// server-rendered overview of clients, mappings, traffic, and configuration.
package serverui

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/view"
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
// sensitive settings mutation. Failed attempts are rate-limited per client IP
// so a stolen session cannot brute-force the web password. The submitted value
// is never logged.
func (s *Server) confirmCurrentPassword(w http.ResponseWriter, r *http.Request, password string) bool {
	ip := shared.ClientIPWithProxy(r, s.trustProxy.Load())
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
	if strings.Contains(err.Error(), "save server config") || strings.Contains(err.Error(), "save config") {
		status = http.StatusInternalServerError
		code = "internal"
		message = "写入配置文件失败"
	}
	if message == "" {
		message = err.Error()
	}
	s.log.Warn("settings mutation failed", "code", code, "error", err)
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
	// sequence. The server's UpdateWebConfig also serializes its persist and
	// SetAdmin step; this lock prevents two handler requests from verifying
	// the same old password and racing each other's session updates.
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
	if request.ConfirmPassword != "" && request.ConfirmPassword != request.NewPassword {
		writeJSONErrorCode(w, http.StatusBadRequest, "password_mismatch", "两次输入的新密码不一致")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(request.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		s.log.Error("hash web password", "error", err)
		writeJSONErrorCode(w, http.StatusInternalServerError, "internal", "无法保存新密码")
		return
	}
	if err := s.api.UpdateWebConfig(username, string(hash)); err != nil {
		s.writeSettingsMutationError(w, err, "invalid_web_config", "更新 Web 管理员失败")
		return
	}
	// UpdateWebConfig persists and updates the live login credentials under
	// the server's settings lock. The handler must not update sessions itself:
	// doing so could overwrite a newer concurrent update.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleUpdatePorts(w http.ResponseWriter, r *http.Request) {
	var request struct {
		passwordRequest
		Allow                 []string `json:"allow"`
		Deny                  []string `json:"deny"`
		AutoRange             string   `json:"auto_range"`
		OfflineReserveSeconds int64    `json:"offline_reserve_seconds"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	if request.OfflineReserveSeconds < 1 {
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_ports", "离线预留时间至少需要 1 秒")
		return
	}
	if err := s.api.UpdatePorts(request.Allow, request.Deny, request.AutoRange, time.Duration(request.OfflineReserveSeconds)*time.Second); err != nil {
		s.writeSettingsMutationError(w, err, "invalid_ports", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.api.Settings())
}

func (s *Server) handleUpdateTrustProxy(w http.ResponseWriter, r *http.Request) {
	var request struct {
		passwordRequest
		TrustProxy bool `json:"trust_proxy"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	if err := s.api.SetTrustProxy(request.TrustProxy); err != nil {
		s.writeSettingsMutationError(w, err, "invalid_proxy_config", "更新代理信任失败")
		return
	}
	// Update the live login/settings-confirmation limiters only after the
	// configuration write succeeded.
	s.SetTrustProxy(request.TrustProxy)
	writeJSON(w, http.StatusOK, s.api.Settings())
}

type rotateCredentialsResponse struct {
	Generation int64                   `json:"generation"`
	Results    []view.CredentialResult `json:"results"`
}

func (s *Server) handleRotateCredentials(w http.ResponseWriter, r *http.Request) {
	var request struct {
		passwordRequest
		Auth         string `json:"auth"`
		ControlToken string `json:"control_token"`
		SyncMode     string `json:"sync_mode"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	syncMode := strings.TrimSpace(request.SyncMode)
	switch syncMode {
	case "":
		syncMode = "manual"
	case "auto", "manual":
	default:
		writeJSONErrorCode(w, http.StatusBadRequest, "invalid_sync_mode", "同步方式必须是 auto 或 manual")
		return
	}
	generation, results, err := s.api.RotateCredentials(request.Auth, request.ControlToken, syncMode)
	if err != nil {
		code := "invalid_credentials"
		switch err.Error() {
		case "unsupported_auth_type":
			code = "unsupported_auth_type"
		case "no_change":
			code = "no_change"
		}
		s.writeSettingsMutationError(w, err, code, err.Error())
		return
	}
	if results == nil {
		results = []view.CredentialResult{}
	}
	writeJSON(w, http.StatusOK, rotateCredentialsResponse{Generation: generation, Results: results})
}

func (s *Server) handleRevokeCredentials(w http.ResponseWriter, r *http.Request) {
	var request passwordRequest
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}
	if err := s.api.RevokePreviousCredentials(); err != nil {
		s.writeSettingsMutationError(w, err, "revoke_failed", "撤销旧凭据失败")
		return
	}
	s.log.Info("previous credentials revoked from web console")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

type revealedCredential struct {
	Generation   int64  `json:"generation"`
	Auth         string `json:"auth"`
	ControlToken string `json:"control_token"`
}

type revealCredentialsResponse struct {
	Current  *revealedCredential `json:"current"`
	Previous *revealedCredential `json:"previous,omitempty"`
}

func (s *Server) handleRevealCredentials(w http.ResponseWriter, r *http.Request) {
	var request passwordRequest
	if !decodeSettingsBody(w, r, &request) {
		return
	}

	if !s.confirmCurrentPassword(w, r, request.secret()) {
		return
	}

	set := s.api.CredentialsSnapshot()
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
	if set.PreviousGeneration > 0 {
		payload.Previous = &revealedCredential{
			Generation:   set.PreviousGeneration,
			Auth:         set.PreviousAuth,
			ControlToken: set.PreviousControlToken,
		}
	}
	// writeJSON sets this too; set it before the body is written so even a
	// later encoder error cannot downgrade the cache policy.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, payload)
}
