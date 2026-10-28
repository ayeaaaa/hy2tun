package serverui

import (
	"errors"
	"net/http"

	"hy2tun/internal/control"
)

func (s *Server) handleDeleteClient(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSONErrorCode(w, http.StatusBadRequest, "bad_request", "client id is required")
		return
	}
	err := s.api.DeleteClient(id)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, control.ErrClientOnline):
		writeJSONErrorCode(w, http.StatusConflict, "client_online", "在线客户端不能删除，请先断开连接")
	case errors.Is(err, control.ErrClientNotFound):
		writeJSONErrorCode(w, http.StatusNotFound, "client_not_found", "客户端不存在或已被删除")
	default:
		s.log.Warn("delete client", "client", id, "error", err)
		writeJSONErrorCode(w, http.StatusInternalServerError, "delete_failed", "删除客户端失败")
	}
}
