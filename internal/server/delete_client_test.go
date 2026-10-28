package server

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/control"
	"hy2tun/internal/ports"
	"hy2tun/internal/state"
)

func testDeleteManager(t *testing.T) (*mappingManager, *state.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(path, time.Hour)
	require.NoError(t, err)
	policy, err := ports.NewPolicy(nil, nil, "20000-20010")
	require.NoError(t, err)
	m := newMappingManager(policy, store, slog.Default(), "", nil, newDataStreams())
	return m, store, path
}

func TestDeleteClientRemovesOfflineClientAndMappings(t *testing.T) {
	m, store, path := testDeleteManager(t)
	store.Upsert(state.ClientState{
		ID: "gone", Name: "old", Online: false,
		Mappings: []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 20001}},
	})
	require.NoError(t, m.deleteClient("gone"))
	_, ok := store.Snapshot().Clients["gone"]
	require.False(t, ok)

	reloaded, err := state.Open(path, time.Hour)
	require.NoError(t, err)
	defer reloaded.Close()
	_, ok = reloaded.Snapshot().Clients["gone"]
	require.False(t, ok)
}

func TestDeleteClientRejectsOnlineClient(t *testing.T) {
	m, store, _ := testDeleteManager(t)
	store.Upsert(state.ClientState{ID: "live", Online: true})
	m.sessions["live"] = &clientSession{id: "live", online: true}
	err := m.deleteClient("live")
	require.ErrorIs(t, err, control.ErrClientOnline)
	_, ok := store.Snapshot().Clients["live"]
	require.True(t, ok)
}

func TestDeleteClientRejectsUnknownClient(t *testing.T) {
	m, _, _ := testDeleteManager(t)
	require.ErrorIs(t, m.deleteClient("missing"), control.ErrClientNotFound)
}
