package manager

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const managerServerBodyLimit = 16 << 10

// serverPayload is the write-only inventory form/API shape. Password is never
// rendered back; an empty value means "keep the stored credential" on update
// and test actions.
type serverPayload struct {
	Name          string `json:"name"`
	BaseURL       string `json:"base_url"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	TLSSkipVerify *bool  `json:"tls_skip_verify"`
}

func (s *WebServer) handleServersPage(w http.ResponseWriter, r *http.Request) {
	data := serversPageData{
		pageBase:            s.page("服务器清单", s.sessions.Username(r)),
		InventoryConfigured: s.inventory != nil,
	}
	if s.inventory != nil {
		data.Servers = s.inventory.List()
	}
	s.render(w, http.StatusOK, s.serversTemplate, data)
}

func (s *WebServer) handleServerPage(w http.ResponseWriter, r *http.Request) {
	if s.inventory == nil {
		http.Error(w, "服务器清单未配置", http.StatusServiceUnavailable)
		return
	}
	view, ok := s.inventory.Get(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.render(w, http.StatusOK, s.serverTemplate, serverPageData{
		pageBase: s.page(view.Name, s.sessions.Username(r)),
		Server:   view,
	})
}

func (s *WebServer) handleListServersAPI(w http.ResponseWriter, r *http.Request) {
	if s.inventory == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "inventory is not configured")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": s.inventory.List()})
}

func (s *WebServer) handleGetServerAPI(w http.ResponseWriter, r *http.Request) {
	if s.inventory == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "inventory is not configured")
		return
	}
	view, ok := s.inventory.Get(r.PathValue("id"))
	if !ok {
		writeJSONError(w, http.StatusNotFound, "server not found")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *WebServer) handleAddServer(w http.ResponseWriter, r *http.Request) {
	inventory, ok := s.inventoryOrError(w)
	if !ok {
		return
	}
	payload, err := decodeServerPayload(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid server request")
		return
	}
	request := AddServerRequest{
		Name:          payload.Name,
		BaseURL:       payload.BaseURL,
		Username:      payload.Username,
		Password:      payload.Password,
		TLSSkipVerify: payload.TLSSkipVerify != nil && *payload.TLSSkipVerify,
	}
	ctx := WithAuditActor(r.Context(), s.sessions.Username(r))
	view, err := inventory.Add(ctx, request)
	if err != nil {
		s.writeInventoryError(w, err)
		return
	}
	w.Header().Set("Location", s.base+"servers/"+view.ID)
	writeJSON(w, http.StatusCreated, view)
}

func (s *WebServer) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	inventory, ok := s.inventoryOrError(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	current, ok := inventory.Get(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "server not found")
		return
	}
	payload, err := decodeServerPayload(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid server request")
		return
	}
	tlsSkipVerify := current.TLSSkipVerify
	if payload.TLSSkipVerify != nil {
		tlsSkipVerify = *payload.TLSSkipVerify
	}
	ctx := WithAuditActor(r.Context(), s.sessions.Username(r))
	if err := inventory.Update(ctx, id, UpdateServerRequest{
		Name:          payload.Name,
		BaseURL:       payload.BaseURL,
		Username:      payload.Username,
		Password:      payload.Password,
		TLSSkipVerify: tlsSkipVerify,
	}); err != nil {
		s.writeInventoryError(w, err)
		return
	}
	view, ok := inventory.Get(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "server not found")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *WebServer) handleRemoveServer(w http.ResponseWriter, r *http.Request) {
	inventory, ok := s.inventoryOrError(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	view, ok := inventory.Get(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "server not found")
		return
	}
	// Deletion is deliberately two-step: the caller must echo the configured
	// display name, so a stray or stale request cannot remove the wrong server.
	if strings.TrimSpace(r.URL.Query().Get("confirm")) != view.Name {
		writeJSONError(w, http.StatusBadRequest, "confirmation does not match the server name")
		return
	}
	ctx := WithAuditActor(r.Context(), s.sessions.Username(r))
	if err := inventory.RemoveWithContext(ctx, id); err != nil {
		s.writeInventoryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *WebServer) handleTestNewServer(w http.ResponseWriter, r *http.Request) {
	inventory, ok := s.inventoryOrError(w)
	if !ok {
		return
	}
	payload, err := decodeServerPayload(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid server request")
		return
	}
	view, err := inventory.TestConnection(r.Context(), AddServerRequest{
		Name:          payload.Name,
		BaseURL:       payload.BaseURL,
		Username:      payload.Username,
		Password:      payload.Password,
		TLSSkipVerify: payload.TLSSkipVerify != nil && *payload.TLSSkipVerify,
	})
	if err != nil {
		s.writeInventoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *WebServer) handleTestStoredServer(w http.ResponseWriter, r *http.Request) {
	inventory, ok := s.inventoryOrError(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	current, ok := inventory.Get(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "server not found")
		return
	}
	payload, err := decodeServerPayload(w, r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid server request")
		return
	}
	tlsSkipVerify := current.TLSSkipVerify
	if payload.TLSSkipVerify != nil {
		tlsSkipVerify = *payload.TLSSkipVerify
	}
	view, err := inventory.TestStoredConnection(r.Context(), id, UpdateServerRequest{
		Name:          payload.Name,
		BaseURL:       payload.BaseURL,
		Username:      payload.Username,
		Password:      payload.Password,
		TLSSkipVerify: tlsSkipVerify,
	})
	if err != nil {
		s.writeInventoryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *WebServer) handleRefreshServer(w http.ResponseWriter, r *http.Request) {
	inventory, ok := s.inventoryOrError(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, ok := inventory.Get(id); !ok {
		writeJSONError(w, http.StatusNotFound, "server not found")
		return
	}
	// A failed refresh still returns the view: the state and LastError fields
	// are the operator-facing result, and the server remains in inventory.
	view, err := inventory.Refresh(r.Context(), id)
	if err != nil {
		s.log.Debug("manual server refresh failed", "server", id, "error", err)
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *WebServer) handleRefreshAllServers(w http.ResponseWriter, r *http.Request) {
	inventory, ok := s.inventoryOrError(w)
	if !ok {
		return
	}
	inventory.RefreshAll(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"servers": inventory.List()})
}

func (s *WebServer) inventoryOrError(w http.ResponseWriter) (*Inventory, bool) {
	if s.inventory == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "inventory is not configured")
		return nil, false
	}
	return s.inventory, true
}

func (s *WebServer) writeInventoryError(w http.ResponseWriter, err error) {
	var validation *ValidationError
	if errors.As(err, &validation) {
		writeJSONError(w, http.StatusBadRequest, validation.Message)
		return
	}
	var connection *ServerConnectionError
	if errors.As(err, &connection) {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": connection.Error(),
			"state": connection.State,
		})
		return
	}
	s.log.Error("manager inventory operation failed", "error", err)
	writeJSONError(w, http.StatusInternalServerError, "inventory operation failed")
}

func decodeServerPayload(w http.ResponseWriter, r *http.Request) (serverPayload, error) {
	r.Body = http.MaxBytesReader(w, r.Body, managerServerBodyLimit)
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		var payload serverPayload
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
			return serverPayload{}, err
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			return serverPayload{}, errors.New("trailing data after JSON object")
		}
		return payload, nil
	}

	if err := r.ParseForm(); err != nil {
		return serverPayload{}, err
	}
	payload := serverPayload{
		Name:     r.PostForm.Get("name"),
		BaseURL:  r.PostForm.Get("base_url"),
		Username: r.PostForm.Get("username"),
		Password: r.PostForm.Get("password"),
	}
	if values, exists := r.PostForm["tls_skip_verify"]; exists {
		value := false
		for _, candidate := range values {
			value = parseFormBool(candidate)
			if value {
				break
			}
		}
		payload.TLSSkipVerify = &value
	}
	return payload, nil
}

func parseFormBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
