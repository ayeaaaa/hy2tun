package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hy2tun/internal/config"
	"hy2tun/internal/manager/upstream"
	"hy2tun/internal/view"
)

const (
	managerClientBodyLimit = 1 << 20
	clientReadTimeout      = 15 * time.Second
	mappingMutationTimeout = 15 * time.Second

	mappingUpdateAction   = "mapping.update"
	mappingExposureAction = "mapping.exposure"
	mappingRefetchAction  = "mapping.refetch"
)

// mappingPushPayload is the manager-side mirror of hy2tund's full-list mapping
// update. Exposures is forwarded unchanged so a new local mapping is created
// through the server's disabled -> local -> enabled transaction.
type mappingPushPayload struct {
	BaseRevision int64                    `json:"base_revision"`
	Mappings     []config.Mapping         `json:"mappings"`
	Exposures    map[string]view.Exposure `json:"exposures"`
}

// clientOperationError is a manager-classified failure that has no upstream
// APIError (for example a mapping edit rejected before it was sent because the
// fresh client detail is offline).
type clientOperationError struct {
	Status  int
	Code    string
	Message string
}

func (e *clientOperationError) Error() string {
	if e == nil {
		return "client operation failed"
	}
	return e.Message
}

// postMutationReadError marks a mutation that the upstream API accepted but
// whose confirming client-detail read failed. The handler records the mutation
// success and the refetch failure as separate audit entries instead of claiming
// the write itself failed.
type postMutationReadError struct {
	Err error
}

func (e *postMutationReadError) Error() string {
	if e == nil || e.Err == nil {
		return "mutation applied but client refresh failed"
	}
	return e.Err.Error()
}

func (e *postMutationReadError) Unwrap() error { return e.Err }

func (s *WebServer) handleClientsPage(w http.ResponseWriter, r *http.Request) {
	if s.inventory == nil {
		http.Error(w, "服务器清单未配置", http.StatusServiceUnavailable)
		return
	}
	serverID := r.PathValue("id")
	serverView, ok := s.inventory.Get(serverID)
	if !ok {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), clientReadTimeout)
	defer cancel()
	var clients []upstream.ClientView
	err := s.inventory.WithClient(ctx, serverID, func(callCtx context.Context, client *upstream.Client) error {
		loaded, readErr := client.Clients(callCtx)
		if readErr != nil {
			return readErr
		}
		clients = loaded
		return nil
	})
	data := clientsPageData{
		pageBase: s.page(serverView.Name+" · 客户端", s.sessions.Username(r)),
		Server:   serverView,
		Clients:  clientRows(clients),
	}
	if err != nil {
		if errors.Is(err, ErrServerNotFound) {
			http.NotFound(w, r)
			return
		}
		data.LoadError = clientErrorMessage(err)
		s.render(w, http.StatusBadGateway, s.clientsTemplate, data)
		return
	}
	s.render(w, http.StatusOK, s.clientsTemplate, data)
}

func (s *WebServer) handleClientPage(w http.ResponseWriter, r *http.Request) {
	if s.inventory == nil {
		http.Error(w, "服务器清单未配置", http.StatusServiceUnavailable)
		return
	}
	serverID := r.PathValue("id")
	clientID := r.PathValue("clientID")
	serverView, ok := s.inventory.Get(serverID)
	if !ok {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), clientReadTimeout)
	defer cancel()
	clientView := upstream.ClientView{ID: clientID}
	err := s.inventory.WithClient(ctx, serverID, func(callCtx context.Context, client *upstream.Client) error {
		loaded, readErr := client.Client(callCtx, clientID)
		if readErr != nil {
			return readErr
		}
		clientView = loaded
		return nil
	})
	data := clientPageData{
		pageBase: s.page(serverView.Name+" · 客户端 "+clientID, s.sessions.Username(r)),
		Server:   serverView,
		Client:   newClientDetailView(clientView),
		Mappings: clientMappingRows(clientView.Mappings),
	}
	if err != nil {
		if errors.Is(err, ErrServerNotFound) || isUpstreamNotFound(err) {
			http.NotFound(w, r)
			return
		}
		data.LoadError = clientErrorMessage(err)
		s.render(w, http.StatusBadGateway, s.clientTemplate, data)
		return
	}
	s.render(w, http.StatusOK, s.clientTemplate, data)
}

func (s *WebServer) handlePushClientMappings(w http.ResponseWriter, r *http.Request) {
	inventory, ok := s.inventoryOrError(w)
	if !ok {
		return
	}
	serverID := r.PathValue("id")
	clientID := r.PathValue("clientID")
	record, ok := inventory.serverRecord(serverID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "server not found")
		return
	}
	payload, err := decodeMappingPushPayload(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid mapping request")
		return
	}
	if err := validatePushExposures(payload.Mappings, payload.Exposures); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := WithAuditActor(r.Context(), s.sessions.Username(r))
	target := mappingPushAuditTarget(clientID, payload.Mappings)
	if !s.beginClientMutationAudit(w, ctx, inventory, record, mappingUpdateAction, target) {
		return
	}

	var refreshed upstream.ClientView
	auditTarget := target
	mutationCtx, cancel := context.WithTimeout(ctx, mappingMutationTimeout)
	defer cancel()
	mutationErr := inventory.WithClient(mutationCtx, serverID, func(callCtx context.Context, client *upstream.Client) error {
		current, readErr := client.Client(callCtx, clientID)
		if readErr != nil {
			return readErr
		}
		// The submitted list does not contain IDs that this edit deletes.
		// Record the union so a successful or failed delete still names the
		// affected mapping in the terminal audit entry.
		auditTarget = mappingPushAuditTargetWithCurrent(clientID, payload.Mappings, current.Mappings)
		if !current.Online {
			return &clientOperationError{
				Status:  http.StatusConflict,
				Code:    upstream.CodeClientOffline,
				Message: "客户端离线，无法修改映射；暴露范围仍可在服务端调整",
			}
		}
		if pushErr := client.PushMappings(callCtx, clientID, payload.BaseRevision, payload.Mappings, payload.Exposures); pushErr != nil {
			return pushErr
		}
		// Always return the revision and runtime state produced by the accepted
		// push; never echo the stale form or a locally incremented revision.
		loaded, readErr := client.Client(callCtx, clientID)
		if readErr != nil {
			return &postMutationReadError{Err: readErr}
		}
		refreshed = loaded
		return nil
	})
	if mutationErr != nil {
		var refetch *postMutationReadError
		if errors.As(mutationErr, &refetch) {
			s.recordAppliedMutationWithRefetchFailure(ctx, inventory, record, mappingUpdateAction, auditTarget, refetch.Err)
			s.writeClientMutationAppliedError(w, refetch.Err)
			return
		}
		s.finishClientMutationAudit(ctx, inventory, record, mappingUpdateAction, auditTarget, "error", mutationErr)
		s.writeClientMutationError(w, mutationErr)
		return
	}
	s.finishClientMutationAudit(ctx, inventory, record, mappingUpdateAction, auditTarget, "ok", nil)
	writeJSON(w, http.StatusOK, refreshed)
}

func (s *WebServer) handleSetMappingExposure(w http.ResponseWriter, r *http.Request) {
	inventory, ok := s.inventoryOrError(w)
	if !ok {
		return
	}
	serverID := r.PathValue("id")
	clientID := r.PathValue("clientID")
	mappingID := r.PathValue("mappingID")
	record, ok := inventory.serverRecord(serverID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "server not found")
		return
	}
	var payload struct {
		Exposure view.Exposure `json:"exposure"`
	}
	if err := decodeClientJSON(w, r, &payload); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid exposure request")
		return
	}
	if !payload.Exposure.Valid() {
		writeJSONError(w, http.StatusBadRequest, "exposure must be public or local")
		return
	}

	ctx := WithAuditActor(r.Context(), s.sessions.Username(r))
	target := mappingExposureAuditTarget(clientID, mappingID)
	if !s.beginClientMutationAudit(w, ctx, inventory, record, mappingExposureAction, target) {
		return
	}

	var refreshed upstream.ClientView
	mutationCtx, cancel := context.WithTimeout(ctx, mappingMutationTimeout)
	defer cancel()
	mutationErr := inventory.WithClient(mutationCtx, serverID, func(callCtx context.Context, client *upstream.Client) error {
		// Exposure is server-side desired state and remains valid while the
		// client is offline. Still fetch detail first so the action always
		// starts from the current client and reports a disappeared client as
		// a 404 instead of a confusing bind error.
		if _, readErr := client.Client(callCtx, clientID); readErr != nil {
			return readErr
		}
		if setErr := client.SetExposure(callCtx, clientID, mappingID, payload.Exposure); setErr != nil {
			return setErr
		}
		loaded, readErr := client.Client(callCtx, clientID)
		if readErr != nil {
			return &postMutationReadError{Err: readErr}
		}
		refreshed = loaded
		return nil
	})
	if mutationErr != nil {
		var refetch *postMutationReadError
		if errors.As(mutationErr, &refetch) {
			s.recordAppliedMutationWithRefetchFailure(ctx, inventory, record, mappingExposureAction, target, refetch.Err)
			s.writeClientMutationAppliedError(w, refetch.Err)
			return
		}
		s.finishClientMutationAudit(ctx, inventory, record, mappingExposureAction, target, "error", mutationErr)
		s.writeClientMutationError(w, mutationErr)
		return
	}
	s.finishClientMutationAudit(ctx, inventory, record, mappingExposureAction, target, "ok", nil)
	writeJSON(w, http.StatusOK, refreshed)
}

func decodeMappingPushPayload(w http.ResponseWriter, r *http.Request) (mappingPushPayload, error) {
	var payload mappingPushPayload
	if err := decodeClientJSON(w, r, &payload); err != nil {
		return mappingPushPayload{}, err
	}
	if payload.Mappings == nil {
		payload.Mappings = []config.Mapping{}
	}
	return payload, nil
}

func decodeClientJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, managerClientBodyLimit)
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if contentType != "" && !strings.HasPrefix(contentType, "application/json") {
		return fmt.Errorf("mapping requests require application/json")
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("trailing data after JSON object")
	}
	return nil
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
			return fmt.Errorf("exposures.%s does not match any mapping", mappingID)
		}
		if !exposure.Valid() {
			return fmt.Errorf("exposures.%s must be public or local", mappingID)
		}
	}
	return nil
}

func mappingPushAuditTarget(clientID string, mappings []config.Mapping) string {
	ids := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		ids = append(ids, mapping.ID)
	}
	return "client:" + clientID + " mappings:" + strings.Join(ids, ",")
}

func mappingPushAuditTargetWithCurrent(clientID string, submitted []config.Mapping, current []upstream.MappingView) string {
	seen := make(map[string]struct{}, len(submitted)+len(current))
	ids := make([]string, 0, len(submitted)+len(current))
	for _, mapping := range submitted {
		if _, ok := seen[mapping.ID]; ok {
			continue
		}
		seen[mapping.ID] = struct{}{}
		ids = append(ids, mapping.ID)
	}
	for _, mapping := range current {
		if _, ok := seen[mapping.ID]; ok {
			continue
		}
		seen[mapping.ID] = struct{}{}
		ids = append(ids, mapping.ID)
	}
	return "client:" + clientID + " mappings:" + strings.Join(ids, ",")
}

func mappingExposureAuditTarget(clientID, mappingID string) string {
	return "client:" + clientID + " mapping:" + mappingID
}

func (s *WebServer) beginClientMutationAudit(w http.ResponseWriter, ctx context.Context, inventory *Inventory, record ServerRecord, action, target string) bool {
	if err := inventory.appendAudit(ctx, AuditEntry{
		ServerID:   record.ID,
		ServerName: record.Name,
		Action:     action,
		Target:     target,
		Result:     "pending",
	}); err != nil {
		s.log.Error("append manager mutation audit entry", "action", action, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "audit log is unavailable")
		return false
	}
	return true
}

func (s *WebServer) finishClientMutationAudit(ctx context.Context, inventory *Inventory, record ServerRecord, action, target, result string, err error) {
	entry := AuditEntry{
		ServerID:   record.ID,
		ServerName: record.Name,
		Action:     action,
		Target:     target,
		Result:     result,
	}
	if err != nil {
		entry.Code = clientErrorCode(err)
		entry.Message = clientErrorMessage(err)
	}
	inventory.appendAuditBestEffort(ctx, entry)
}

// recordAppliedMutationWithRefetchFailure keeps the accepted mutation and the
// confirming read failure as separate terminal audit records.
func (s *WebServer) recordAppliedMutationWithRefetchFailure(ctx context.Context, inventory *Inventory, record ServerRecord, action, target string, refetchErr error) {
	s.finishClientMutationAudit(ctx, inventory, record, action, target, "ok", nil)
	s.finishClientMutationAudit(ctx, inventory, record, mappingRefetchAction, target, "error", refetchErr)
}

func clientErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	var apiError *upstream.APIError
	if errors.As(err, &apiError) {
		if message := strings.TrimSpace(apiError.Message); message != "" {
			return message
		}
	}
	return err.Error()
}

func clientErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var operation *clientOperationError
	if errors.As(err, &operation) && operation.Code != "" {
		return operation.Code
	}
	var apiError *upstream.APIError
	if errors.As(err, &apiError) {
		if apiError.Code != "" {
			return apiError.Code
		}
		switch apiError.Status {
		case http.StatusUnauthorized:
			return "auth_error"
		case http.StatusForbidden:
			return "forbidden"
		case http.StatusBadRequest:
			return "invalid_request"
		case http.StatusNotFound:
			return "not_found"
		case http.StatusConflict:
			return upstream.CodeConflict
		case http.StatusTooManyRequests:
			return "rate_limited"
		}
	}
	switch {
	case errors.Is(err, ErrServerNotFound):
		return "server_not_found"
	case errors.Is(err, context.DeadlineExceeded):
		return "upstream_timeout"
	default:
		return "upstream_error"
	}
}

func (s *WebServer) writeClientMutationError(w http.ResponseWriter, err error) {
	status, code, message, reload, retryAfter := classifyClientMutationError(err)
	message = clientRetryMessage(message, retryAfter, w)
	body := map[string]any{"error": message}
	if code != "" {
		body["code"] = code
	}
	if reload {
		body["reload"] = true
	}
	addClientErrorDetails(body, err)
	writeJSON(w, status, body)
}

// writeClientMutationAppliedError reports a mutation that the upstream API
// accepted but whose confirming read failed. It must not claim the write failed
// or tell the operator to retry the stale form as if nothing was applied.
func (s *WebServer) writeClientMutationAppliedError(w http.ResponseWriter, err error) {
	status, code, message, _, retryAfter := classifyClientMutationError(err)
	if status == http.StatusBadRequest || status == http.StatusConflict || status == http.StatusNotFound {
		status = http.StatusBadGateway
	}
	message = clientRetryMessage(message, retryAfter, w)
	body := map[string]any{
		"error":   "变更已保存，但读取最新客户端状态失败：" + message,
		"applied": true,
		"reload":  true,
	}
	if code != "" {
		body["code"] = code
	}
	addClientErrorDetails(body, err)
	writeJSON(w, status, body)
}

func clientRetryMessage(message string, retryAfter time.Duration, w http.ResponseWriter) string {
	if retryAfter <= 0 {
		return message
	}
	seconds := int64((retryAfter + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	return fmt.Sprintf("%s（%d 秒后可重试）", message, seconds)
}

func addClientErrorDetails(body map[string]any, err error) {
	var apiError *upstream.APIError
	if !errors.As(err, &apiError) {
		return
	}
	if apiError.Revision != 0 {
		body["revision"] = apiError.Revision
	}
	if apiError.Partial {
		body["partial"] = true
	}
	if len(apiError.Results) > 0 {
		body["results"] = apiError.Results
	}
	if strings.TrimSpace(string(apiError.ActiveExposure)) != "" {
		body["active_exposure"] = apiError.ActiveExposure
	}
	if len(apiError.UnavailableFamilies) > 0 {
		body["unavailable_families"] = apiError.UnavailableFamilies
	}
	if apiError.Closed {
		body["closed"] = true
	}
}

func classifyClientMutationError(err error) (status int, code, message string, reload bool, retryAfter time.Duration) {
	message = clientErrorMessage(err)
	code = clientErrorCode(err)
	var operation *clientOperationError
	if errors.As(err, &operation) {
		status = operation.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		return status, code, message, status == http.StatusConflict, 0
	}
	var apiError *upstream.APIError
	if errors.As(err, &apiError) {
		switch {
		case apiError.Status == http.StatusBadRequest:
			return http.StatusBadRequest, code, message, false, 0
		case apiError.Status == http.StatusNotFound:
			return http.StatusNotFound, code, message, true, 0
		case apiError.Status == http.StatusConflict:
			return http.StatusConflict, code, message, true, 0
		case apiError.Status == http.StatusTooManyRequests:
			return http.StatusTooManyRequests, code, message, false, apiError.RetryAfter
		case apiError.Status == http.StatusUnauthorized || apiError.Status == http.StatusForbidden:
			return http.StatusBadGateway, code, message, false, 0
		default:
			return http.StatusBadGateway, code, message, false, 0
		}
	}
	if errors.Is(err, ErrServerNotFound) {
		return http.StatusNotFound, code, "server not found", false, 0
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, code, "upstream request timed out", true, 0
	}
	return http.StatusBadGateway, code, message, false, 0
}

func isUpstreamNotFound(err error) bool {
	var apiError *upstream.APIError
	return errors.As(err, &apiError) && apiError.Status == http.StatusNotFound
}
