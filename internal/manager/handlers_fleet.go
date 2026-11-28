package manager

import (
	"net/http"
)

type fleetClientsPageData struct {
	pageBase
	Clients             []FleetClientView
	Errors              []FleetClientError
	InventoryConfigured bool
}

type fleetClientPageData struct {
	pageBase
	Client  FleetClientView
	Errors  []FleetClientError
	Missing bool
}

func (s *WebServer) handleFleetClientsPage(w http.ResponseWriter, r *http.Request) {
	data := fleetClientsPageData{
		pageBase:            s.page("客户端", s.sessions.Username(r)),
		InventoryConfigured: s.inventory != nil,
	}
	if s.inventory != nil {
		data.Clients, data.Errors = s.inventory.FleetClients(r.Context())
	}
	s.render(w, http.StatusOK, s.fleetClientsTemplate, data)
}

func (s *WebServer) handleFleetClientPage(w http.ResponseWriter, r *http.Request) {
	if s.inventory == nil {
		http.Error(w, "服务器清单未配置", http.StatusServiceUnavailable)
		return
	}
	key := r.PathValue("clientID")
	clients, errs := s.inventory.FleetClients(r.Context())
	client, ok := FindFleetClient(clients, key)
	data := fleetClientPageData{
		pageBase: s.page("客户端 "+key, s.sessions.Username(r)),
		Client:   client,
		Errors:   errs,
		Missing:  !ok,
	}
	if !ok {
		data.pageBase.Title = "客户端不存在"
		s.render(w, http.StatusNotFound, s.fleetClientTemplate, data)
		return
	}
	s.render(w, http.StatusOK, s.fleetClientTemplate, data)
}
