package clientui

import (
	"errors"
	"net/http"
	"strings"

	"hy2tun/internal/config"
)

type proxyAPI interface {
	Proxy() ProxyView
	UpdateProxy(config.ProxyConfig) error
}

func (s *Server) handleUpdateProxy(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled bool   `json:"enabled"`
		SOCKS5  string `json:"socks5"`
		HTTP    string `json:"http"`
	}
	if !decodeSettingsBody(w, r, &request) {
		return
	}
	proxy := config.ProxyConfig{
		Enabled: request.Enabled,
		SOCKS5:  strings.TrimSpace(request.SOCKS5),
		HTTP:    strings.TrimSpace(request.HTTP),
	}

	name := strings.TrimSpace(r.PathValue("name"))
	var target API
	var err error
	if name != "" {
		if s.multi == nil {
			http.Error(w, "server not found", http.StatusNotFound)
			return
		}
		target, _ = s.multi.Server(name)
		if target == nil {
			http.Error(w, "server not found", http.StatusNotFound)
			return
		}
		err = s.multi.UpdateProxy(name, proxy)
	} else {
		target, _ = s.apiFor(r)
		if target == nil {
			http.Error(w, "server not found", http.StatusNotFound)
			return
		}
		if s.multi != nil {
			name = strings.TrimSpace(r.URL.Query().Get("server"))
			if name == "" {
				name = "default"
			}
			err = s.multi.UpdateProxy(name, proxy)
		} else if updater, ok := target.(proxyAPI); ok {
			err = updater.UpdateProxy(proxy)
		} else {
			http.Error(w, "proxy is not supported", http.StatusInternalServerError)
			return
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, config.ErrProxyConflict) || strings.Contains(err.Error(), "conflict"):
			writeJSONErrorCode(w, http.StatusConflict, "proxy_conflict", err.Error())
		case strings.Contains(err.Error(), "proxy."):
			writeJSONErrorCode(w, http.StatusBadRequest, "invalid_proxy", err.Error())
		default:
			s.log.Warn("client proxy update failed", "error", err)
			writeJSONErrorCode(w, http.StatusInternalServerError, "proxy_failed", "更新代理失败")
		}
		return
	}
	writeJSON(w, http.StatusOK, s.settingsFor(target))
}
