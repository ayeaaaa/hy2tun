package manager

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"hy2tun/internal/webui/shared"
)

const (
	managerPasswordBodyLimit = 64 << 10
	managerPasswordMinRunes  = 8
	managerPasswordMaxBytes  = 72
)

type accountPageData struct {
	pageBase
}

type changePasswordPayload struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
}

// handleAccountPage renders the single administrator account page. The page
// uses the manager's normal auth/CSRF-protected JSON form path.
func (s *WebServer) handleAccountPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, s.accountTemplate, accountPageData{
		pageBase: s.page("账户", s.sessions.Username(r)),
	})
}

// handleChangePassword verifies the current password, stores a new bcrypt hash,
// updates the live login handler, and invalidates every existing manager
// session. The operator must log in again with the new password.
func (s *WebServer) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	ip := shared.ClientIPWithProxy(r, s.cfg.TrustProxy)
	if s.passwordLimiter != nil && !s.passwordLimiter.Allow(ip) {
		w.Header().Set("Retry-After", "60")
		writeJSONError(w, http.StatusTooManyRequests, "尝试次数过多，请稍后再试")
		return
	}

	var payload changePasswordPayload
	r.Body = http.MaxBytesReader(w, r.Body, managerPasswordBodyLimit)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体不是有效的 JSON")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "请求体包含多余内容")
		return
	}

	admin := s.store.State().Admin
	if s.passwordLimiter != nil {
		if bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(payload.CurrentPassword)) != nil {
			s.passwordLimiter.Failure(ip)
			writeJSONError(w, http.StatusForbidden, "当前密码不正确")
			return
		}
	} else if bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(payload.CurrentPassword)) != nil {
		writeJSONError(w, http.StatusForbidden, "当前密码不正确")
		return
	}

	if utf8.RuneCountInString(payload.NewPassword) < managerPasswordMinRunes {
		writeJSONError(w, http.StatusBadRequest, "新密码至少需要 8 个字符")
		return
	}
	if len(payload.NewPassword) > managerPasswordMaxBytes {
		writeJSONError(w, http.StatusBadRequest, "新密码不能超过 72 字节")
		return
	}
	if payload.ConfirmPassword != "" && payload.ConfirmPassword != payload.NewPassword {
		writeJSONError(w, http.StatusBadRequest, "两次输入的新密码不一致")
		return
	}

	action := AuditEntry{
		Actor:  admin.Username,
		Action: "manager.password",
		Target: admin.Username,
		Result: "pending",
	}
	if s.audit == nil {
		writeJSONError(w, http.StatusInternalServerError, "审计日志未配置")
		return
	}
	if err := s.audit.Append(action); err != nil {
		s.log.Error("audit manager password change", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "无法写入审计日志，密码未修改")
		return
	}

	storeErr := s.store.SetAdminPassword(payload.NewPassword)
	if storeErr != nil && !errors.Is(storeErr, ErrStateCommitted) {
		action.Result = "error"
		action.Code = "store_error"
		_ = s.audit.Append(action)
		s.log.Error("store manager password", "error", storeErr)
		writeJSONError(w, http.StatusInternalServerError, "无法保存新密码")
		return
	}

	admin = s.store.State().Admin
	s.login.SetAdmin(admin.Username, admin.PasswordHash)
	s.sessions.RevokeAll()
	s.sessions.Clear(w, r)

	action.Result = "ok"
	if errors.Is(storeErr, ErrStateCommitted) {
		action.Code = "durability_warning"
	}
	_ = s.audit.Append(action)
	if s.passwordLimiter != nil {
		s.passwordLimiter.Success(ip)
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
