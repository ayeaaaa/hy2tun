package manager

import "strings"

// serversPageData backs the inventory management page. Servers is empty when
// the shell has no inventory or when no servers are configured.
type serversPageData struct {
	pageBase
	Servers             []ServerView
	InventoryConfigured bool
}

// serverPageData backs one server's summary/edit page.
type serverPageData struct {
	pageBase
	Server ServerView
}

// StateLabel is the operator-facing label for the inventory state.
func (v ServerView) StateLabel() string {
	switch v.State {
	case ServerStateOnline:
		return "在线"
	case ServerStateAuthError:
		return "认证失败"
	case ServerStateUnreachable:
		return "无法连接"
	case ServerStateUnsupported:
		return "部分不支持"
	default:
		return "尚未刷新"
	}
}

// StateClass is the CSS modifier for the state badge.
func (v ServerView) StateClass() string {
	switch v.State {
	case ServerStateOnline:
		return "ok"
	case ServerStateAuthError, ServerStateUnreachable:
		return "bad"
	case ServerStateUnsupported:
		return "warn"
	default:
		return "muted"
	}
}

// ModeLabel renders an upstream mode compactly.
func (v ServerView) ModeLabel() string {
	mode := strings.TrimSpace(v.Mode)
	if mode == "" {
		return "—"
	}
	switch mode {
	case "hy2":
		return "HY2"
	case "tcp":
		return "TCP"
	case "hy2+tcp":
		return "HY2 + TCP"
	default:
		return mode
	}
}

// HasLastError reports whether the last refresh produced an operator-visible
// error.
func (v ServerView) HasLastError() bool { return strings.TrimSpace(v.LastError) != "" }
