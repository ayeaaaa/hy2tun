package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"hy2tun/internal/config"
	"hy2tun/internal/manager/upstream"
)

const (
	settingsReadTimeout      = 15 * time.Second
	settingsMutationTimeout  = 15 * time.Second
	settingsRotationTimeout  = 30 * time.Second
	settingsBodyLimit        = 64 << 10
	settingsRevokeConfirmVal = "revoke"

	settingsModeAction          = "settings.mode"
	settingsPortsAction         = "settings.ports"
	settingsWebAction           = "settings.web"
	settingsWebCredentialsError = "settings.web.credentials"
	settingsWebReloginAction    = "settings.web.relogin"
	settingsProxyAction         = "settings.trust_proxy"
	settingsRotateAction        = "settings.credentials.rotate"
	settingsRevokeAction        = "settings.credentials.revoke"
	settingsRefreshAction       = "settings.refresh"
)

type settingsModePayload struct {
	Mode config.TransportMode `json:"mode"`
}

type settingsPortsPayload struct {
	Allow                 []string `json:"allow"`
	Deny                  []string `json:"deny"`
	AutoRange             string   `json:"auto_range"`
	OfflineReserveSeconds int64    `json:"offline_reserve_seconds"`
}

type settingsWebPayload struct {
	Username        string `json:"username"`
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"confirm_password"`
}

type settingsTrustProxyPayload struct {
	TrustProxy *bool `json:"trust_proxy"`
}

type settingsRotatePayload struct {
	Auth         string `json:"auth"`
	ControlToken string `json:"control_token"`
	SyncMode     string `json:"sync_mode"`
}

// settingsOperationError is a manager-classified settings failure that has no
// upstream APIError (validation, local store, or audit failure).
type settingsOperationError struct {
	Status  int
	Code    string
	Message string
}

func (e *settingsOperationError) Error() string {
	if e == nil {
		return "settings operation failed"
	}
	return e.Message
}

// settingsActionResponse is the JSON success shape. The refreshed, secret-free
// settings snapshot is promoted to the top level; rotation adds the generation
// and per-client results, and a partial success adds Applied/Warning instead of
// pretending the mutation failed.
type settingsActionResponse struct {
	upstream.Settings
	Generation int64                       `json:"generation,omitempty"`
	Results    []upstream.CredentialResult `json:"results,omitempty"`
	Applied    bool                        `json:"applied,omitempty"`
	Warning    string                      `json:"warning,omitempty"`
}

type settingsMutationOutcome struct {
	Settings    upstream.Settings
	HasSettings bool
	Generation  int64
	Results     []upstream.CredentialResult
	Applied     bool
	Warning     string
	// SkipReload avoids another upstream login attempt after a credential
	// change that the manager could not re-authenticate. The operator must
	// repair the stored credential first.
	SkipReload bool
}

func (s *WebServer) handleServerSettingsPage(w http.ResponseWriter, r *http.Request) {
	if s.inventory == nil {
		http.Error(w, "服务器清单未配置", http.StatusServiceUnavailable)
		return
	}
	server, ok := s.inventory.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), settingsReadTimeout)
	defer cancel()
	settings, err := s.loadServerSettings(ctx, s.inventory, server.ID)
	data := s.newSettingsPageData(server, settingsFormValues{}, r)
	data.LoadedAt = time.Now().UTC()
	if err != nil {
		if errors.Is(err, ErrServerNotFound) {
			http.NotFound(w, r)
			return
		}
		s.log.Debug("read server settings failed", "server", server.ID, "error", err)
		data.LoadError = settingsErrorMessage(err)
		s.render(w, settingsErrorStatus(err), s.settingsTemplate, data)
		return
	}
	data.Settings = newServerSettingsView(settings)
	data.Form = settingsFormFromSettings(settings)
	data.ClientRows = credentialClientRows(settings.Clients)
	data.HasSettings = true
	s.render(w, http.StatusOK, s.settingsTemplate, data)
}

// handleGetServerSettingsAPI proxies GET /api/settings for read-only display.
// The response is the same secret-free snapshot the server's own API returns.
func (s *WebServer) handleGetServerSettingsAPI(w http.ResponseWriter, r *http.Request) {
	inventory, record, ok := s.settingsMutationServer(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), settingsReadTimeout)
	defer cancel()
	settings, err := s.loadServerSettings(ctx, inventory, record.ID)
	if err != nil {
		s.writeSettingsError(w, r, inventory, record, settingsFormValues{}, err, false)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *WebServer) handleUpdateServerMode(w http.ResponseWriter, r *http.Request) {
	inventory, record, ok := s.settingsMutationServer(w, r)
	if !ok {
		return
	}
	payload, form, err := decodeModeSettings(w, r)
	if err != nil {
		s.writeSettingsError(w, r, inventory, record, form, err, false)
		return
	}
	if err := validateModeSettings(&payload); err != nil {
		s.writeSettingsError(w, r, inventory, record, form, err, false)
		return
	}
	ctx, ok := s.beginSettingsMutation(w, r, inventory, record, form,
		settingsModeAction, "mode:"+string(payload.Mode))
	if !ok {
		return
	}
	s.runSettingsMutation(w, r, ctx, inventory, record, form,
		settingsMutationTimeout, settingsModeAction, "mode:"+string(payload.Mode),
		func(callCtx context.Context, client *upstream.Client) error {
			return client.UpdateMode(callCtx, payload.Mode)
		})
}

func (s *WebServer) handleUpdateServerPorts(w http.ResponseWriter, r *http.Request) {
	inventory, record, ok := s.settingsMutationServer(w, r)
	if !ok {
		return
	}
	payload, form, err := decodePortsSettings(w, r)
	if err != nil {
		s.writeSettingsError(w, r, inventory, record, form, err, false)
		return
	}
	if payload.OfflineReserveSeconds < 1 {
		s.writeSettingsError(w, r, inventory, record, form,
			&settingsOperationError{Status: http.StatusBadRequest, Code: "invalid_ports", Message: "离线预留时间至少需要 1 秒。"}, false)
		return
	}
	ctx, ok := s.beginSettingsMutation(w, r, inventory, record, form, settingsPortsAction, "ports")
	if !ok {
		return
	}
	s.runSettingsMutation(w, r, ctx, inventory, record, form,
		settingsMutationTimeout, settingsPortsAction, "ports",
		func(callCtx context.Context, client *upstream.Client) error {
			return client.UpdatePorts(callCtx, payload.Allow, payload.Deny, payload.AutoRange,
				time.Duration(payload.OfflineReserveSeconds)*time.Second)
		})
}

func (s *WebServer) handleUpdateServerTrustProxy(w http.ResponseWriter, r *http.Request) {
	inventory, record, ok := s.settingsMutationServer(w, r)
	if !ok {
		return
	}
	payload, form, err := decodeTrustProxySettings(w, r)
	if err != nil {
		s.writeSettingsError(w, r, inventory, record, form, err, false)
		return
	}
	target := "trust_proxy:" + strconv.FormatBool(*payload.TrustProxy)
	ctx, ok := s.beginSettingsMutation(w, r, inventory, record, form, settingsProxyAction, target)
	if !ok {
		return
	}
	trust := *payload.TrustProxy
	s.runSettingsMutation(w, r, ctx, inventory, record, form,
		settingsMutationTimeout, settingsProxyAction, target,
		func(callCtx context.Context, client *upstream.Client) error {
			return client.SetTrustProxy(callCtx, trust)
		})
}

func (s *WebServer) handleRotateServerCredentials(w http.ResponseWriter, r *http.Request) {
	inventory, record, ok := s.settingsMutationServer(w, r)
	if !ok {
		return
	}
	payload, form, err := decodeRotateSettings(w, r)
	if err != nil {
		s.writeSettingsError(w, r, inventory, record, form, err, false)
		return
	}
	if err := validateRotateSettings(&payload); err != nil {
		s.writeSettingsError(w, r, inventory, record, form, err, false)
		return
	}
	target := "sync:" + payload.SyncMode
	ctx, ok := s.beginSettingsMutation(w, r, inventory, record, form, settingsRotateAction, target)
	if !ok {
		return
	}
	s.runSettingsMutationOutcome(w, r, ctx, inventory, record, form,
		settingsRotationTimeout, settingsRotateAction, target,
		func(callCtx context.Context, client *upstream.Client, outcome *settingsMutationOutcome) error {
			result, err := client.RotateCredentials(callCtx, payload.Auth, payload.ControlToken, payload.SyncMode)
			if err != nil {
				return err
			}
			outcome.Generation = result.Generation
			outcome.Results = sanitizeCredentialResults(result.Results, payload.Auth, payload.ControlToken)
			return nil
		})
}

func (s *WebServer) handleRevokeServerCredentials(w http.ResponseWriter, r *http.Request) {
	inventory, record, ok := s.settingsMutationServer(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(r.URL.Query().Get("confirm")) != settingsRevokeConfirmVal {
		s.writeSettingsError(w, r, inventory, record, settingsFormValues{},
			&settingsOperationError{
				Status:  http.StatusBadRequest,
				Code:    "confirmation_required",
				Message: "撤销旧凭据需要显式确认。",
			}, false)
		return
	}
	ctx, ok := s.beginSettingsMutation(w, r, inventory, record, settingsFormValues{},
		settingsRevokeAction, "previous_generation")
	if !ok {
		return
	}
	s.runSettingsMutation(w, r, ctx, inventory, record, settingsFormValues{},
		settingsRotationTimeout, settingsRevokeAction, "previous_generation",
		func(callCtx context.Context, client *upstream.Client) error {
			return client.RevokeCredentials(callCtx)
		})
}

// handleUpdateServerWebAdmin changes the upstream Web administrator. The
// current password is read from the manager's encrypted store, never from the
// browser. After the upstream accepts the change, the new password (and
// username, when changed) is persisted and a fresh upstream client must
// authenticate with it. The old client is never used after the change.
func (s *WebServer) handleUpdateServerWebAdmin(w http.ResponseWriter, r *http.Request) {
	inventory, record, ok := s.settingsMutationServer(w, r)
	if !ok {
		return
	}
	payload, form, err := decodeWebSettings(w, r)
	if err != nil {
		s.writeSettingsError(w, r, inventory, record, form, err, false)
		return
	}
	if err := validateWebSettings(&payload); err != nil {
		s.writeSettingsError(w, r, inventory, record, form, err, false)
		return
	}

	target := "web_admin:" + payload.Username
	ctx, ok := s.beginSettingsMutation(w, r, inventory, record, form, settingsWebAction, target)
	if !ok {
		return
	}

	mutationCtx, cancel := context.WithTimeout(ctx, settingsMutationTimeout)
	defer cancel()
	var storeErr error
	mutationErr := inventory.WithClient(mutationCtx, record.ID, func(callCtx context.Context, client *upstream.Client) error {
		currentPassword, readErr := inventory.store.ServerPassword(record.ID)
		if readErr != nil {
			return &settingsOperationError{
				Status:  http.StatusInternalServerError,
				Code:    "credential_unavailable",
				Message: "无法读取已保存的上游管理员密码；请先在“编辑服务器”中更新上游凭据。",
			}
		}
		if updateErr := client.UpdateWebAdmin(callCtx, payload.Username,
			string(currentPassword), payload.NewPassword); updateErr != nil {
			return updateErr
		}
		// The upstream accepted the change. Persist the new password before
		// doing anything else; the old password is no longer valid.
		if saveErr := inventory.store.SetServerPassword(record.ID, []byte(payload.NewPassword)); saveErr != nil {
			storeErr = saveErr
			return nil
		}
		if payload.Username != record.UpstreamUsername {
			updated := record
			updated.UpstreamUsername = payload.Username
			updated.UpdatedAt = time.Now().UTC()
			if saveErr := inventory.store.UpdateServer(updated, nil); saveErr != nil {
				storeErr = saveErr
			}
		}
		return nil
	})
	if mutationErr != nil {
		s.finishSettingsAudit(ctx, inventory, record, settingsWebAction, target, "error", mutationErr)
		s.writeSettingsError(w, r, inventory, record, form, mutationErr, false)
		return
	}
	s.finishSettingsAudit(ctx, inventory, record, settingsWebAction, target, "ok", nil)
	if storeErr != nil {
		s.log.Error("store upstream password after web admin change", "server", record.ID, "error", storeErr)
		storeFailure := &settingsOperationError{
			Status:  http.StatusInternalServerError,
			Code:    "credential_store_failed",
			Message: "上游服务器已接受新的 Web 管理员凭据，但管理器保存新的上游凭据失败；请立即在“编辑服务器”中更新上游凭据。",
		}
		s.finishSettingsAudit(ctx, inventory, record, settingsWebCredentialsError, target, "error", storeFailure)
		// Drop the stale cached client and mark the server if the remaining
		// stored credential no longer authenticates.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), settingsReadTimeout)
		_, _ = inventory.Refresh(cleanupCtx, record.ID)
		cleanupCancel()
		s.writeSettingsError(w, r, inventory, record, form, storeFailure, true)
		return
	}

	// Re-authenticate with the changed ciphertext. Refresh detects the
	// credential change and replaces the cached client instead of reusing the
	// old session.
	followCtx, followCancel := context.WithTimeout(context.WithoutCancel(ctx), settingsRotationTimeout)
	defer followCancel()
	view, reloginErr := inventory.Refresh(followCtx, record.ID)
	if reloginErr != nil && view.State == ServerStateAuthError {
		s.finishSettingsAudit(ctx, inventory, record, settingsWebReloginAction, target, "error", reloginErr)
		s.writeSettingsSuccess(w, r, inventory, record, form, settingsMutationOutcome{
			Applied:    true,
			SkipReload: true,
			Warning:    "Web 管理员已更新，但使用新凭据重新登录失败；服务器已标记为认证失败。请在“编辑服务器”中更新上游凭据。",
		})
		return
	}

	settings, loadErr := s.loadServerSettings(followCtx, inventory, record.ID)
	outcome := settingsMutationOutcome{Applied: true}
	if loadErr != nil {
		s.finishSettingsAudit(ctx, inventory, record, settingsRefreshAction, target, "error", loadErr)
		outcome.Warning = "Web 管理员已更新，但读取最新设置失败：" + settingsErrorMessage(loadErr)
		s.writeSettingsSuccess(w, r, inventory, record, form, outcome)
		return
	}
	outcome.Settings = settings
	outcome.HasSettings = true
	s.writeSettingsSuccess(w, r, inventory, record, form, outcome)
}

func (s *WebServer) settingsMutationServer(w http.ResponseWriter, r *http.Request) (*Inventory, ServerRecord, bool) {
	inventory, ok := s.inventoryOrError(w)
	if !ok {
		return nil, ServerRecord{}, false
	}
	record, ok := inventory.serverRecord(r.PathValue("id"))
	if !ok {
		writeJSONError(w, http.StatusNotFound, "server not found")
		return nil, ServerRecord{}, false
	}
	return inventory, record, true
}

func (s *WebServer) loadServerSettings(ctx context.Context, inventory *Inventory, serverID string) (upstream.Settings, error) {
	var settings upstream.Settings
	err := inventory.WithClient(ctx, serverID, func(callCtx context.Context, client *upstream.Client) error {
		loaded, readErr := client.Settings(callCtx)
		if readErr != nil {
			return readErr
		}
		settings = sanitizeSettingsSnapshot(loaded)
		return nil
	})
	return settings, err
}

func (s *WebServer) beginSettingsMutation(w http.ResponseWriter, r *http.Request, inventory *Inventory, record ServerRecord, form settingsFormValues, action, target string) (context.Context, bool) {
	ctx := WithAuditActor(r.Context(), s.sessions.Username(r))
	if err := inventory.appendAudit(ctx, AuditEntry{
		ServerID:   record.ID,
		ServerName: record.Name,
		Action:     action,
		Target:     target,
		Result:     "pending",
	}); err != nil {
		s.log.Error("append manager settings audit entry", "action", action, "error", err)
		s.writeSettingsError(w, r, inventory, record, form,
			&settingsOperationError{
				Status:  http.StatusInternalServerError,
				Code:    "audit_unavailable",
				Message: "audit log is unavailable",
			}, false)
		return nil, false
	}
	return ctx, true
}

// settingsInvoke runs one upstream mutation. Rotation needs to capture its
// response; all other mutations use the plain form.
func (s *WebServer) runSettingsMutation(
	w http.ResponseWriter,
	r *http.Request,
	ctx context.Context,
	inventory *Inventory,
	record ServerRecord,
	form settingsFormValues,
	timeout time.Duration,
	action, target string,
	invoke func(context.Context, *upstream.Client) error,
) {
	s.runSettingsMutationOutcome(w, r, ctx, inventory, record, form, timeout, action, target,
		func(callCtx context.Context, client *upstream.Client, _ *settingsMutationOutcome) error {
			return invoke(callCtx, client)
		})
}

func (s *WebServer) runSettingsMutationOutcome(
	w http.ResponseWriter,
	r *http.Request,
	ctx context.Context,
	inventory *Inventory,
	record ServerRecord,
	form settingsFormValues,
	timeout time.Duration,
	action, target string,
	invoke func(context.Context, *upstream.Client, *settingsMutationOutcome) error,
) {
	outcome := settingsMutationOutcome{}
	mutationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var refreshErr error
	err := inventory.WithClient(mutationCtx, record.ID, func(callCtx context.Context, client *upstream.Client) error {
		if invokeErr := invoke(callCtx, client, &outcome); invokeErr != nil {
			return invokeErr
		}
		loaded, readErr := client.Settings(callCtx)
		if readErr != nil {
			refreshErr = readErr
			return nil
		}
		outcome.Settings = sanitizeSettingsSnapshot(loaded)
		outcome.HasSettings = true
		return nil
	})
	if err != nil {
		s.finishSettingsAudit(ctx, inventory, record, action, target, "error", err)
		s.writeSettingsError(w, r, inventory, record, form, err, false)
		return
	}

	s.finishSettingsAudit(ctx, inventory, record, action, target, "ok", nil)
	if refreshErr != nil {
		s.finishSettingsAudit(ctx, inventory, record, settingsRefreshAction, target, "error", refreshErr)
		outcome.Applied = true
		outcome.Warning = "设置已保存，但读取最新设置失败：" + settingsErrorMessage(refreshErr)
	}

	// Refresh inventory display data after success, but never turn a failed
	// display refresh into a false mutation failure.
	refreshCtx, refreshCancel := context.WithTimeout(context.WithoutCancel(ctx), settingsReadTimeout)
	if _, refreshViewErr := inventory.Refresh(refreshCtx, record.ID); refreshViewErr != nil {
		s.log.Debug("refresh server after settings mutation failed", "server", record.ID, "error", refreshViewErr)
	}
	refreshCancel()

	s.writeSettingsSuccess(w, r, inventory, record, form, outcome)
}

func (s *WebServer) finishSettingsAudit(ctx context.Context, inventory *Inventory, record ServerRecord, action, target, result string, err error) {
	entry := AuditEntry{
		ServerID:   record.ID,
		ServerName: record.Name,
		Action:     action,
		Target:     target,
		Result:     result,
	}
	if err != nil {
		entry.Code = settingsErrorCode(err)
		entry.Message = settingsErrorMessage(err)
	}
	inventory.appendAuditBestEffort(ctx, entry)
}

func (s *WebServer) writeSettingsSuccess(w http.ResponseWriter, r *http.Request, inventory *Inventory, record ServerRecord, form settingsFormValues, outcome settingsMutationOutcome) {
	if !wantsSettingsHTML(r) {
		writeJSON(w, http.StatusOK, settingsActionResponse{
			Settings:   outcome.Settings,
			Generation: outcome.Generation,
			Results:    outcome.Results,
			Applied:    outcome.Applied,
			Warning:    outcome.Warning,
		})
		return
	}

	settings := outcome.Settings
	hasSettings := outcome.HasSettings
	if !hasSettings && !outcome.SkipReload {
		ctx, cancel := context.WithTimeout(r.Context(), settingsReadTimeout)
		loaded, err := s.loadServerSettings(ctx, inventory, record.ID)
		cancel()
		if err == nil {
			settings = loaded
			hasSettings = true
		}
	}
	data := s.newSettingsPageData(s.currentServerView(inventory, record), form, r)
	if hasSettings {
		data.Settings = newServerSettingsView(settings)
		data.Form = settingsFormFromSettings(settings)
		data.ClientRows = credentialClientRows(settings.Clients)
		data.HasSettings = true
	} else {
		data.Form = form
	}
	if outcome.Generation > 0 || len(outcome.Results) > 0 {
		data.Rotation = newSettingsRotationView(outcome.Generation, outcome.Results)
	}
	data.Notice = "服务器设置已保存，并已重新读取最新配置。"
	data.Warning = outcome.Warning
	s.render(w, http.StatusOK, s.settingsTemplate, data)
}

func (s *WebServer) writeSettingsError(w http.ResponseWriter, r *http.Request, inventory *Inventory, record ServerRecord, form settingsFormValues, err error, applied bool) {
	status, code, message := settingsErrorDetails(err)
	if retryAfter := settingsRetryAfter(err); retryAfter > 0 {
		seconds := int64((retryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}

	if wantsSettingsHTML(r) {
		settings := upstream.Settings{}
		hasSettings := false
		loadError := ""
		if !applied {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), settingsReadTimeout)
			loaded, loadErr := s.loadServerSettings(ctx, inventory, record.ID)
			cancel()
			if loadErr == nil {
				settings = loaded
				hasSettings = true
			} else {
				loadError = settingsErrorMessage(loadErr)
			}
		}
		data := s.newSettingsPageData(s.currentServerView(inventory, record), form, r)
		if hasSettings {
			data.Settings = newServerSettingsView(settings)
			data.Form = mergeSettingsForm(settingsFormFromSettings(settings), form)
			data.ClientRows = credentialClientRows(settings.Clients)
			data.HasSettings = true
		} else {
			data.Form = form
		}
		data.Error = message
		data.LoadError = loadError
		s.render(w, status, s.settingsTemplate, data)
		return
	}

	body := map[string]any{"error": message}
	if code != "" {
		body["code"] = code
	}
	if applied {
		body["applied"] = true
	}
	writeJSON(w, status, body)
}

func (s *WebServer) newSettingsPageData(server ServerView, form settingsFormValues, r *http.Request) serverSettingsPageData {
	return serverSettingsPageData{
		pageBase: s.page(server.Name+" · 服务器设置", s.sessions.Username(r)),
		Server:   server,
		Form:     form,
		LoadedAt: time.Now().UTC(),
	}
}

func recordView(record ServerRecord) ServerView {
	return viewFromRecord(record)
}

func (s *WebServer) currentServerView(inventory *Inventory, record ServerRecord) ServerView {
	if view, ok := inventory.Get(record.ID); ok {
		return view
	}
	return recordView(record)
}

func wantsSettingsHTML(r *http.Request) bool {
	if !strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") {
		return false
	}
	return !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "application/json")
}

func decodeModeSettings(w http.ResponseWriter, r *http.Request) (settingsModePayload, settingsFormValues, error) {
	if isJSONSettingsRequest(r) {
		var payload settingsModePayload
		if err := decodeSettingsJSON(w, r, &payload); err != nil {
			return payload, settingsFormValues{Kind: "mode"}, err
		}
		return payload, settingsFormValues{Kind: "mode", Mode: string(payload.Mode)}, nil
	}
	values, err := parseSettingsForm(w, r)
	if err != nil {
		return settingsModePayload{}, settingsFormValues{Kind: "mode"}, err
	}
	form := settingsFormValues{Kind: "mode", Mode: strings.TrimSpace(values.Get("mode"))}
	return settingsModePayload{Mode: config.TransportMode(form.Mode)}, form, nil
}

func validateModeSettings(payload *settingsModePayload) error {
	mode := config.TransportMode(strings.TrimSpace(string(payload.Mode)))
	if mode == "" {
		return &settingsOperationError{Status: http.StatusBadRequest, Code: "invalid_mode", Message: "请选择传输模式。"}
	}
	if err := mode.Validate(); err != nil {
		return &settingsOperationError{Status: http.StatusBadRequest, Code: "invalid_mode", Message: "传输模式无效。"}
	}
	payload.Mode = mode
	return nil
}

func decodePortsSettings(w http.ResponseWriter, r *http.Request) (settingsPortsPayload, settingsFormValues, error) {
	if isJSONSettingsRequest(r) {
		var payload settingsPortsPayload
		if err := decodeSettingsJSON(w, r, &payload); err != nil {
			return payload, settingsFormValues{Kind: "ports"}, err
		}
		form := settingsFormValues{
			Kind:                  "ports",
			AllowText:             strings.Join(payload.Allow, ", "),
			DenyText:              strings.Join(payload.Deny, ", "),
			AutoRange:             payload.AutoRange,
			OfflineReserveSeconds: payload.OfflineReserveSeconds,
		}
		return payload, form, nil
	}
	values, err := parseSettingsForm(w, r)
	if err != nil {
		return settingsPortsPayload{}, settingsFormValues{Kind: "ports"}, err
	}
	form := settingsFormValues{
		Kind:      "ports",
		AllowText: strings.TrimSpace(values.Get("allow")),
		DenyText:  strings.TrimSpace(values.Get("deny")),
		AutoRange: strings.TrimSpace(values.Get("auto_range")),
	}
	form.OfflineReserveSeconds, _ = strconv.ParseInt(strings.TrimSpace(values.Get("offline_reserve_seconds")), 10, 64)
	payload := settingsPortsPayload{
		Allow:                 splitPortList(form.AllowText),
		Deny:                  splitPortList(form.DenyText),
		AutoRange:             form.AutoRange,
		OfflineReserveSeconds: form.OfflineReserveSeconds,
	}
	if payload.Allow == nil {
		payload.Allow = []string{}
	}
	if payload.Deny == nil {
		payload.Deny = []string{}
	}
	return payload, form, nil
}

func decodeWebSettings(w http.ResponseWriter, r *http.Request) (settingsWebPayload, settingsFormValues, error) {
	if isJSONSettingsRequest(r) {
		var payload settingsWebPayload
		if err := decodeSettingsJSON(w, r, &payload); err != nil {
			return payload, settingsFormValues{Kind: "web"}, err
		}
		return payload, settingsFormValues{Kind: "web", WebUsername: payload.Username}, nil
	}
	values, err := parseSettingsForm(w, r)
	if err != nil {
		return settingsWebPayload{}, settingsFormValues{Kind: "web"}, err
	}
	payload := settingsWebPayload{
		Username:        strings.TrimSpace(values.Get("username")),
		NewPassword:     values.Get("new_password"),
		ConfirmPassword: values.Get("confirm_password"),
	}
	return payload, settingsFormValues{Kind: "web", WebUsername: payload.Username}, nil
}

func validateWebSettings(payload *settingsWebPayload) error {
	username := strings.TrimSpace(payload.Username)
	if username == "" {
		return &settingsOperationError{Status: http.StatusBadRequest, Code: "username_required", Message: "Web 管理员用户名不能为空。"}
	}
	if len([]rune(payload.NewPassword)) < 8 {
		return &settingsOperationError{Status: http.StatusBadRequest, Code: "weak_password", Message: "新密码至少需要 8 个字符。"}
	}
	if len([]byte(payload.NewPassword)) > 72 {
		return &settingsOperationError{Status: http.StatusBadRequest, Code: "weak_password", Message: "新密码不能超过 72 字节。"}
	}
	if payload.ConfirmPassword == "" || payload.ConfirmPassword != payload.NewPassword {
		return &settingsOperationError{Status: http.StatusBadRequest, Code: "password_mismatch", Message: "两次输入的新密码不一致。"}
	}
	payload.Username = username
	// The current password is always taken from the manager's encrypted
	// store; a submitted value is ignored and never forwarded.
	payload.CurrentPassword = ""
	return nil
}

func decodeTrustProxySettings(w http.ResponseWriter, r *http.Request) (settingsTrustProxyPayload, settingsFormValues, error) {
	if isJSONSettingsRequest(r) {
		var payload settingsTrustProxyPayload
		if err := decodeSettingsJSON(w, r, &payload); err != nil {
			return payload, settingsFormValues{Kind: "proxy"}, err
		}
		if payload.TrustProxy == nil {
			return payload, settingsFormValues{Kind: "proxy"},
				&settingsOperationError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "trust_proxy is required"}
		}
		return payload, settingsFormValues{Kind: "proxy", TrustProxy: *payload.TrustProxy}, nil
	}
	values, err := parseSettingsForm(w, r)
	if err != nil {
		return settingsTrustProxyPayload{}, settingsFormValues{Kind: "proxy"}, err
	}
	trust := parseFormBool(values.Get("trust_proxy"))
	return settingsTrustProxyPayload{TrustProxy: &trust}, settingsFormValues{Kind: "proxy", TrustProxy: trust}, nil
}

func decodeRotateSettings(w http.ResponseWriter, r *http.Request) (settingsRotatePayload, settingsFormValues, error) {
	if isJSONSettingsRequest(r) {
		var payload settingsRotatePayload
		if err := decodeSettingsJSON(w, r, &payload); err != nil {
			return payload, settingsFormValues{Kind: "rotate", SyncMode: settingsDefaultSyncMode}, err
		}
		return payload, settingsFormValues{Kind: "rotate", SyncMode: payload.SyncMode}, nil
	}
	values, err := parseSettingsForm(w, r)
	if err != nil {
		return settingsRotatePayload{}, settingsFormValues{Kind: "rotate", SyncMode: settingsDefaultSyncMode}, err
	}
	payload := settingsRotatePayload{
		Auth:         values.Get("auth"),
		ControlToken: values.Get("control_token"),
		SyncMode:     strings.TrimSpace(values.Get("sync_mode")),
	}
	return payload, settingsFormValues{Kind: "rotate", SyncMode: payload.SyncMode}, nil
}

func validateRotateSettings(payload *settingsRotatePayload) error {
	// Credential values are intentionally not trimmed: a deliberate leading
	// or trailing character is part of the new secret.
	if len(payload.Auth) < 8 {
		return &settingsOperationError{Status: http.StatusBadRequest, Code: "invalid_credentials", Message: "新的 HY2 密码至少需要 8 个字符。"}
	}
	if len(payload.ControlToken) < 16 {
		return &settingsOperationError{Status: http.StatusBadRequest, Code: "invalid_credentials", Message: "新的 control token 至少需要 16 个字符。"}
	}
	syncMode := strings.TrimSpace(payload.SyncMode)
	switch syncMode {
	case "auto", "manual":
		payload.SyncMode = syncMode
		return nil
	default:
		return &settingsOperationError{Status: http.StatusBadRequest, Code: "invalid_sync_mode", Message: "同步方式必须是 auto 或 manual。"}
	}
}

func isJSONSettingsRequest(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))), "application/json")
}

func decodeSettingsJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, settingsBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return &settingsOperationError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "请求体不是有效的 JSON。"}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return &settingsOperationError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "请求体包含多余内容。"}
	}
	return nil
}

func parseSettingsForm(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	r.Body = http.MaxBytesReader(w, r.Body, settingsBodyLimit)
	if err := r.ParseForm(); err != nil {
		return nil, &settingsOperationError{Status: http.StatusBadRequest, Code: "invalid_request", Message: "请求体不是有效的表单。"}
	}
	return r.PostForm, nil
}

func splitPortList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.FieldsFunc(value, func(character rune) bool {
		switch character {
		case ',', '\n', '\r', ' ', '\t':
			return true
		default:
			return false
		}
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func settingsErrorDetails(err error) (status int, code, message string) {
	status = settingsErrorStatus(err)
	code = settingsErrorCode(err)
	message = settingsErrorMessage(err)
	return status, code, message
}

func settingsErrorStatus(err error) int {
	var operation *settingsOperationError
	if errors.As(err, &operation) {
		if operation.Status > 0 {
			return operation.Status
		}
		return http.StatusBadGateway
	}
	var apiError *upstream.APIError
	if errors.As(err, &apiError) {
		switch apiError.Status {
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return http.StatusBadRequest
		case http.StatusUnauthorized, http.StatusForbidden:
			return http.StatusBadGateway
		case http.StatusNotFound:
			return http.StatusNotFound
		case http.StatusConflict:
			return http.StatusConflict
		case http.StatusTooManyRequests:
			return http.StatusTooManyRequests
		case http.StatusNotImplemented:
			return http.StatusBadGateway
		default:
			if apiError.Status >= 500 {
				return http.StatusBadGateway
			}
			return http.StatusBadRequest
		}
	}
	switch {
	case errors.Is(err, ErrServerNotFound):
		return http.StatusNotFound
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled):
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}

func settingsErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var operation *settingsOperationError
	if errors.As(err, &operation) && operation.Code != "" {
		return operation.Code
	}
	var apiError *upstream.APIError
	if errors.As(err, &apiError) {
		if apiError.Code != "" {
			return apiError.Code
		}
		switch apiError.Status {
		case http.StatusBadRequest:
			return "invalid_request"
		case http.StatusUnauthorized:
			return "auth_error"
		case http.StatusForbidden:
			return "forbidden"
		case http.StatusNotFound:
			return "not_found"
		case http.StatusConflict:
			return "conflict"
		case http.StatusTooManyRequests:
			return "rate_limited"
		case http.StatusNotImplemented:
			return "unsupported"
		default:
			return "upstream_error"
		}
	}
	switch {
	case errors.Is(err, ErrServerNotFound):
		return "server_not_found"
	case errors.Is(err, context.DeadlineExceeded):
		return "upstream_timeout"
	case errors.Is(err, context.Canceled):
		return "upstream_canceled"
	default:
		return "upstream_error"
	}
}

func settingsErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	var operation *settingsOperationError
	if errors.As(err, &operation) {
		return operation.Message
	}
	var apiError *upstream.APIError
	if errors.As(err, &apiError) {
		if message := settingsUpstreamMessage(apiError); message != "" {
			return message
		}
		if message := strings.TrimSpace(apiError.Message); message != "" {
			return message
		}
		return http.StatusText(apiError.Status)
	}
	switch {
	case errors.Is(err, ErrServerNotFound):
		return "server not found"
	case errors.Is(err, context.DeadlineExceeded):
		return "上游请求超时。"
	case errors.Is(err, context.Canceled):
		return "上游请求已取消。"
	default:
		return err.Error()
	}
}

func settingsUpstreamMessage(apiError *upstream.APIError) string {
	code := strings.TrimSpace(apiError.Code)
	upstreamMessage := strings.TrimSpace(apiError.Message)
	switch code {
	case "invalid_password":
		return "当前上游 Web 管理员密码不正确；未更新管理器保存的凭据。请先在服务器原生控制台确认密码，或通过“编辑服务器”更新凭据。"
	case "rate_limited":
		if apiError.RetryAfter > 0 {
			seconds := int64((apiError.RetryAfter + time.Second - 1) / time.Second)
			return fmt.Sprintf("上游服务器限流，请稍后重试（%d 秒后可重试）。", seconds)
		}
		return "上游服务器限流，请稍后重试。"
	case "invalid_mode":
		return "传输模式无效。"
	case "invalid_ports":
		if upstreamMessage != "" {
			return "端口策略无效：" + upstreamMessage
		}
		return "端口策略无效。"
	case "invalid_web_config":
		return "更新上游 Web 管理员失败。"
	case "username_required":
		return "Web 管理员用户名不能为空。"
	case "weak_password":
		return "新密码需要至少 8 个字符且不超过 72 字节。"
	case "password_mismatch":
		return "两次输入的新密码不一致。"
	case "invalid_sync_mode":
		return "同步方式必须是 auto 或 manual。"
	case "invalid_credentials":
		if upstreamMessage != "" {
			return "新凭据无效：" + upstreamMessage
		}
		return "新凭据无效。"
	case "unsupported_auth_type":
		return "上游 HY2 认证方式不是 password，无法在线轮换凭据。"
	case "no_change":
		return "新凭据与当前凭据相同，未创建新代际。"
	case "revoke_failed":
		if upstreamMessage != "" {
			return "撤销旧凭据失败：" + upstreamMessage
		}
		return "撤销旧凭据失败。"
	case "internal":
		return "上游服务器写入配置失败。"
	case "not_implemented", "unsupported":
		return "该上游服务器版本不支持此在线设置。"
	case "invalid_body", "invalid_request":
		return "上游服务器拒绝了请求格式。"
	}

	switch apiError.Status {
	case http.StatusUnauthorized:
		return "上游登录失败，请检查服务器凭据。"
	case http.StatusForbidden:
		return "上游拒绝该操作（权限或来源校验失败）。"
	case http.StatusNotFound:
		return "上游端点或资源不存在。"
	case http.StatusTooManyRequests:
		base := upstreamMessage
		if base == "" {
			base = "上游服务器限流，请稍后重试"
		}
		if apiError.RetryAfter > 0 {
			seconds := int64((apiError.RetryAfter + time.Second - 1) / time.Second)
			return fmt.Sprintf("%s（%d 秒后可重试）。", base, seconds)
		}
		return base
	case http.StatusNotImplemented:
		return "该上游服务器版本不支持此在线设置。"
	}
	return ""
}

// sanitizeSettingsSnapshot re-masks the upstream credential fields. The
// server's API is secret-free by design, but the manager does not rely on that
// guarantee for anything it renders or serializes.
func sanitizeSettingsSnapshot(settings upstream.Settings) upstream.Settings {
	settings.Credentials.CurrentAuthMasked = maskSettingsSecret(settings.Credentials.CurrentAuthMasked)
	settings.Credentials.CurrentControlTokenMasked = maskSettingsSecret(settings.Credentials.CurrentControlTokenMasked)
	settings.Credentials.PreviousAuthMasked = maskSettingsSecret(settings.Credentials.PreviousAuthMasked)
	settings.Credentials.PreviousControlTokenMasked = maskSettingsSecret(settings.Credentials.PreviousControlTokenMasked)
	return settings
}

// sanitizeCredentialResults removes any submitted rotation secret from
// per-client error text before it reaches the response, HTML, or audit.
func sanitizeCredentialResults(results []upstream.CredentialResult, secrets ...string) []upstream.CredentialResult {
	if len(results) == 0 {
		return results
	}
	out := make([]upstream.CredentialResult, len(results))
	copy(out, results)
	for index := range out {
		for _, secret := range secrets {
			if secret == "" {
				continue
			}
			out[index].Error = strings.ReplaceAll(out[index].Error, secret, "[REDACTED]")
			if encoded := url.QueryEscape(secret); encoded != secret {
				out[index].Error = strings.ReplaceAll(out[index].Error, encoded, "[REDACTED]")
			}
		}
	}
	return out
}

func settingsRetryAfter(err error) time.Duration {
	var apiError *upstream.APIError
	if errors.As(err, &apiError) {
		return apiError.RetryAfter
	}
	return 0
}
