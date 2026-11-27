package manager

import (
	"net/url"
	"strconv"
	"strings"

	"hy2tun/internal/manager/upstream"
	"hy2tun/internal/view"
)

// clientsPageData backs GET /servers/{id}/clients. LoadError is set when the
// upstream read failed; the server view and a retry link remain visible so the
// page is still useful during an outage.
type clientsPageData struct {
	pageBase
	Server    ServerView
	Clients   []clientRowView
	LoadError string
}

// clientPageData backs GET /servers/{id}/clients/{clientID}. Mappings is always
// rebuilt from the fresh upstream detail fetched for this request.
type clientPageData struct {
	pageBase
	Server    ServerView
	Client    clientDetailView
	Mappings  []clientMappingView
	LoadError string
}

// clientRowView decorates one client-list row without adding any secret field.
type clientRowView struct {
	upstream.ClientView
}

func clientRows(clients []upstream.ClientView) []clientRowView {
	rows := make([]clientRowView, 0, len(clients))
	for _, client := range clients {
		rows = append(rows, clientRowView{ClientView: client})
	}
	return rows
}

func (c clientRowView) DisplayName() string {
	if name := strings.TrimSpace(c.Name); name != "" {
		return name
	}
	return c.ID
}

func (c clientRowView) PathID() string { return url.PathEscape(c.ID) }

func (c clientRowView) OnlineLabel() string {
	if c.Online {
		return "在线"
	}
	return "离线"
}

func (c clientRowView) StateClass() string {
	if c.Online {
		return "ok"
	}
	return "muted"
}

func (c clientRowView) MappingCount() int { return len(c.Mappings) }

// clientDetailView is the fresh GET /api/clients/{id} read model with display
// helpers. It deliberately exposes no upstream credential or session data.
type clientDetailView struct {
	upstream.ClientView
}

func newClientDetailView(client upstream.ClientView) clientDetailView {
	return clientDetailView{ClientView: client}
}

func (c clientDetailView) DisplayName() string {
	if name := strings.TrimSpace(c.Name); name != "" {
		return name
	}
	return c.ID
}

func (c clientDetailView) PathID() string { return url.PathEscape(c.ID) }

func (c clientDetailView) OnlineLabel() string {
	if c.Online {
		return "在线"
	}
	return "离线"
}

func (c clientDetailView) StateClass() string {
	if c.Online {
		return "ok"
	}
	return "muted"
}

func (c clientDetailView) MappingCount() int { return len(c.Mappings) }

func (c clientDetailView) ReadOnly() bool { return !c.Online }

func (c clientDetailView) TransportLabel() string {
	switch strings.ToLower(strings.TrimSpace(c.Transport)) {
	case "tcp":
		return "TCP（UDP over TCP）"
	case "hy2":
		return "HY2"
	case "":
		return "—"
	default:
		return c.Transport
	}
}

// clientMappingView decorates one mapping row with exposure labels. Exposure is
// the server-selected state; an empty value is the legacy public default.
type clientMappingView struct {
	upstream.MappingView
}

func clientMappingRows(mappings []upstream.MappingView) []clientMappingView {
	rows := make([]clientMappingView, 0, len(mappings))
	for _, mapping := range mappings {
		rows = append(rows, clientMappingView{MappingView: mapping})
	}
	return rows
}

func (m clientMappingView) PathID() string { return url.PathEscape(m.ID) }

func (m clientMappingView) ExposureValue() view.Exposure {
	if m.Exposure == "" {
		return view.ExposurePublic
	}
	return m.Exposure
}

func (m clientMappingView) IsPublic() bool { return m.ExposureValue() == view.ExposurePublic }

func (m clientMappingView) IsLocal() bool { return m.ExposureValue() == view.ExposureLocal }

func (m clientMappingView) ExposureLabel() string {
	if m.IsLocal() {
		return "仅本机"
	}
	return "公网"
}

func (m clientMappingView) EnabledLabel() string {
	if m.Enabled {
		return "已启用"
	}
	return "已禁用"
}

func (m clientMappingView) HasError() bool { return strings.TrimSpace(m.Error) != "" }

func (m clientMappingView) RemotePortLabel() string {
	if m.RemotePort == 0 {
		return "自动"
	}
	return strconv.Itoa(m.RemotePort)
}
