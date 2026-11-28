package manager

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"time"

	"hy2tun/internal/manager/upstream"
)

const fleetReadTimeout = 30 * time.Second

// FleetClientServerView is one server connection belonging to a client.
// BaseURL identifies the operator-visible upstream server; the UI can show it
// alongside the stable manager server ID/name.
type FleetClientServerView struct {
	ServerID   string
	ServerName string
	BaseURL    string
	Online     bool
	Version    string
	Mappings   int
	LastSeen   time.Time
}

// FleetClientView is the client-centric aggregate. ID is the upstream client
// ID used as the global identity; Name is display-only. A same ID with
// different names is marked Conflict rather than silently merged.
type FleetClientView struct {
	ID           string
	Name         string
	Version      string
	Online       bool
	MappingCount int
	Servers      []FleetClientServerView
	Conflict     bool
}

// FleetClientError reports a server whose client list could not be read. The
// rest of the fleet remains visible.
type FleetClientError struct {
	ServerID   string
	ServerName string
	Message    string
}

// FleetClients reads the client list from every configured server and groups
// records by stable client ID. A record with no ID is grouped by name under a
// synthetic key so it is still visible, but the group is marked ungrouped.
func (i *Inventory) FleetClients(ctx context.Context) ([]FleetClientView, []FleetClientError) {
	if i == nil || i.store == nil {
		return []FleetClientView{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, fleetReadTimeout)
	defer cancel()

	servers := i.List()
	aggregated := make(map[string]*FleetClientView)
	order := make([]string, 0)
	var errs []FleetClientError

	for _, server := range servers {
		server := server
		var clients []upstream.ClientView
		err := i.WithClient(ctx, server.ID, func(callCtx context.Context, client *upstream.Client) error {
			loaded, readErr := client.Clients(callCtx)
			if readErr != nil {
				return readErr
			}
			clients = loaded
			return nil
		})
		if err != nil {
			errs = append(errs, FleetClientError{
				ServerID:   server.ID,
				ServerName: server.Name,
				Message:    clientErrorMessage(err),
			})
			continue
		}
		for _, client := range clients {
			key := strings.TrimSpace(client.ID)
			ungrouped := key == ""
			if ungrouped {
				key = "name:" + strings.TrimSpace(client.Name)
				if key == "name:" {
					key = "unnamed:" + server.ID
				}
			}
			entry, ok := aggregated[key]
			if !ok {
				entry = &FleetClientView{
					ID:      client.ID,
					Name:    strings.TrimSpace(client.Name),
					Version: client.Version,
				}
				aggregated[key] = entry
				order = append(order, key)
			}
			if name := strings.TrimSpace(client.Name); name != "" {
				if entry.Name == "" {
					entry.Name = name
				} else if entry.Name != name {
					entry.Conflict = true
				}
			}
			entry.Online = entry.Online || client.Online
			entry.MappingCount += len(client.Mappings)
			if entry.Version == "" {
				entry.Version = client.Version
			}
			entry.Servers = append(entry.Servers, FleetClientServerView{
				ServerID:   server.ID,
				ServerName: server.Name,
				BaseURL:    server.BaseURL,
				Online:     client.Online,
				Version:    client.Version,
				Mappings:   len(client.Mappings),
				LastSeen:   client.LastSeen,
			})
		}
	}

	clients := make([]FleetClientView, 0, len(order))
	for _, key := range order {
		entry := aggregated[key]
		sort.Slice(entry.Servers, func(a, b int) bool {
			if entry.Servers[a].ServerName == entry.Servers[b].ServerName {
				return entry.Servers[a].ServerID < entry.Servers[b].ServerID
			}
			return entry.Servers[a].ServerName < entry.Servers[b].ServerName
		})
		clients = append(clients, *entry)
	}
	sort.Slice(clients, func(a, b int) bool {
		left, right := clients[a].DisplayName(), clients[b].DisplayName()
		if left == right {
			return clients[a].ID < clients[b].ID
		}
		return left < right
	})
	return clients, errs
}

// FindFleetClient finds one aggregate row by its stable client key.
func FindFleetClient(clients []FleetClientView, key string) (FleetClientView, bool) {
	for _, client := range clients {
		if client.ID == key {
			return client, true
		}
	}
	return FleetClientView{}, false
}

func (c FleetClientView) DisplayName() string {
	if name := strings.TrimSpace(c.Name); name != "" {
		return name
	}
	if c.ID != "" {
		return c.ID
	}
	return "未命名客户端"
}

func (c FleetClientView) StateLabel() string {
	if c.Online {
		return "在线"
	}
	return "离线"
}

func (c FleetClientView) StateClass() string {
	if c.Online {
		return "ok"
	}
	return "muted"
}

func (c FleetClientView) PathID() string { return url.PathEscape(c.ID) }

func (s FleetClientServerView) StateLabel() string {
	if s.Online {
		return "在线"
	}
	return "离线"
}

func (s FleetClientServerView) StateClass() string {
	if s.Online {
		return "ok"
	}
	return "muted"
}

func (s FleetClientServerView) ClientPath() string {
	return url.PathEscape(s.ServerID)
}
