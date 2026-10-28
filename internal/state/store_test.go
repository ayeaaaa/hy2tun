package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"hy2tun/internal/config"

	"github.com/stretchr/testify/require"
)

func TestStoreAtomicSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, 24*time.Hour)
	require.NoError(t, err)
	s.Upsert(ClientState{
		ID: "c1", Name: "home", Version: "0.1.0", Revision: 2,
		Mappings: []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222, LocalAddr: "127.0.0.1:22", Enabled: true}},
	})
	require.NoError(t, s.Save())
	require.NoError(t, s.Close())

	s2, err := Open(path, 24*time.Hour)
	require.NoError(t, err)
	defer s2.Close()
	got := s2.Snapshot().Clients["c1"]
	require.NotNil(t, got)
	require.Equal(t, "home", got.Name)
	require.Equal(t, int64(2), got.Revision)
}

func TestReservationExpires(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"), time.Hour)
	require.NoError(t, err)
	defer s.Close()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s.Upsert(ClientState{
		ID: "c1", Online: false,
		Mappings:      []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222}},
		ReservedUntil: now.Add(time.Hour),
	})
	require.True(t, s.Reserved("c1", 2222, now))
	require.False(t, s.Reserved("c1", 2222, now.Add(2*time.Hour)))
}

func TestOpenMissingFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, time.Hour)
	require.NoError(t, err)
	require.Empty(t, s.Snapshot().Clients)
	require.NoError(t, s.Close())
}

func TestOpenCorruptedFileReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, os.WriteFile(path, []byte("{bad json"), 0600))

	s, err := Open(path, time.Hour)
	require.Nil(t, s)
	require.Error(t, err)
	require.Contains(t, err.Error(), "state: parse "+path)
}

func TestSnapshotIsDeepCopy(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"), time.Hour)
	require.NoError(t, err)
	defer s.Close()

	s.Upsert(ClientState{
		ID:       "c1",
		Mappings: []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222}},
	})
	snap := s.Snapshot()
	snap.Clients["c1"].Name = "mutated"
	snap.Clients["c1"].Mappings[0].RemotePort = 9999

	got := s.Snapshot().Clients["c1"]
	require.Empty(t, got.Name)
	require.Equal(t, 2222, got.Mappings[0].RemotePort)
}

func TestUpsertCopiesCallerState(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"), time.Hour)
	require.NoError(t, err)
	defer s.Close()

	mappings := []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222}}
	s.Upsert(ClientState{ID: "c1", Name: "home", Mappings: mappings})
	mappings[0].RemotePort = 9999

	got := s.Snapshot().Clients["c1"]
	require.Equal(t, "home", got.Name)
	require.Equal(t, 2222, got.Mappings[0].RemotePort)
}

func TestSaveKeepsPreviousFileAsBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, time.Hour)
	require.NoError(t, err)
	defer s.Close()

	s.Upsert(ClientState{ID: "c1", Name: "first"})
	require.NoError(t, s.Save())

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	s.Upsert(ClientState{ID: "c1", Name: "second"})
	require.NoError(t, s.Save())

	current, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(current), `"second"`)

	backup, err := os.ReadFile(path + ".bak")
	require.NoError(t, err)
	require.Contains(t, string(backup), `"first"`)

	backupInfo, err := os.Stat(path + ".bak")
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), backupInfo.Mode().Perm())
}

func TestSetOnlineReservesPortForOfflineClient(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"), 2*time.Hour)
	require.NoError(t, err)
	defer s.Close()

	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s.Upsert(ClientState{
		ID: "c1", Online: true,
		Mappings: []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222}},
	})

	s.SetOnline("c1", false, now)
	got := s.Snapshot().Clients["c1"]
	require.False(t, got.Online)
	require.Equal(t, now, got.LastSeen)
	require.Equal(t, now.Add(2*time.Hour), got.ReservedUntil)

	require.True(t, s.Reserved("c1", 2222, now))
	require.True(t, s.Reserved("c1", 2222, now.Add(2*time.Hour-time.Nanosecond)))
	require.False(t, s.Reserved("c1", 2222, now.Add(2*time.Hour)))
}

func TestReservedRequiresOfflineClientAndKnownPort(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"), time.Hour)
	require.NoError(t, err)
	defer s.Close()

	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	s.Upsert(ClientState{
		ID: "c1", Online: true,
		Mappings:      []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222}},
		ReservedUntil: now.Add(time.Hour),
	})

	require.False(t, s.Reserved("c1", 2222, now), "an online client must not hold a reservation")
	require.False(t, s.Reserved("missing", 2222, now))
	require.False(t, s.Reserved("c1", 2223, now), "port must belong to the client's mappings")

	s.SetOnline("c1", false, now)
	require.True(t, s.Reserved("c1", 2222, now))
	require.False(t, s.Reserved("c1", 2223, now))
}

func TestOpenEmptyPathKeepsStateInMemory(t *testing.T) {
	s, err := Open("", time.Hour)
	require.NoError(t, err)

	s.Upsert(ClientState{ID: "c1", Name: "home"})
	require.NoError(t, s.Save())
	require.NoError(t, s.Close())

	require.Equal(t, "home", s.Snapshot().Clients["c1"].Name)
}

func TestCloseFlushesPendingChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, time.Hour)
	require.NoError(t, err)
	s.Upsert(ClientState{ID: "c1", Name: "home"})
	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "Close must be idempotent")

	reopened, err := Open(path, time.Hour)
	require.NoError(t, err)
	defer reopened.Close()
	require.Equal(t, "home", reopened.Snapshot().Clients["c1"].Name)
}

func TestDeleteRemovesClientAndReservation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.json"), time.Hour)
	require.NoError(t, err)
	defer s.Close()

	s.Upsert(ClientState{
		ID: "gone", Online: false,
		Mappings:      []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222}},
		ReservedUntil: time.Now().Add(time.Hour),
	})
	require.True(t, s.Reserved("gone", 2222, time.Now()))
	require.True(t, s.Delete("gone"))
	_, ok := s.Snapshot().Clients["gone"]
	require.False(t, ok)
	require.False(t, s.Reserved("gone", 2222, time.Now()))
	require.False(t, s.Delete("gone"))
}
