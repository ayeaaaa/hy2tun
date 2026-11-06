package state

import (
	"encoding/json"
	"errors"
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

func TestClientStateTransportRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, time.Hour)
	require.NoError(t, err)
	s.Upsert(ClientState{ID: "c1", Name: "home", Online: false, Transport: "tcp"})
	require.NoError(t, s.Save())
	require.NoError(t, s.Close())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), `"transport": "tcp"`)

	s2, err := Open(path, time.Hour)
	require.NoError(t, err)
	defer s2.Close()
	got := s2.Snapshot().Clients["c1"]
	require.NotNil(t, got)
	require.False(t, got.Online)
	require.Equal(t, "tcp", got.Transport)
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

func TestPruneOfflineRemovesOnlyExpiredOfflineClients(t *testing.T) {
	s, err := Open("", time.Hour)
	require.NoError(t, err)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	s.Upsert(ClientState{ID: "old", Online: false, LastSeen: now.Add(-8 * 24 * time.Hour)})
	s.Upsert(ClientState{ID: "recent", Online: false, LastSeen: now.Add(-time.Hour)})
	s.Upsert(ClientState{ID: "online", Online: true, LastSeen: now.Add(-8 * 24 * time.Hour)})

	require.Equal(t, 1, s.PruneOffline(now, 7*24*time.Hour))
	snapshot := s.Snapshot()
	require.NotContains(t, snapshot.Clients, "old")
	require.Contains(t, snapshot.Clients, "recent")
	require.Contains(t, snapshot.Clients, "online")
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
	deleted, err := s.DeleteAndSave("gone")
	require.NoError(t, err)
	require.True(t, deleted)
	_, ok := s.Snapshot().Clients["gone"]
	require.False(t, ok)
	require.False(t, s.Reserved("gone", 2222, time.Now()))
	deleted, err = s.DeleteAndSave("gone")
	require.NoError(t, err)
	require.False(t, deleted)
}

func TestDeleteAndSaveFailureRestoresClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "state.json")
	s, err := Open(path, time.Hour)
	require.NoError(t, err)
	defer s.Close()

	s.Upsert(ClientState{ID: "gone", Name: "old", Mappings: []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222}}})
	deleted, err := s.DeleteAndSave("gone")
	require.Error(t, err)
	require.False(t, deleted)
	_, ok := s.Snapshot().Clients["gone"]
	require.True(t, ok)

	// Once the directory is writable, a retry can complete.
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	deleted, err = s.DeleteAndSave("gone")
	require.NoError(t, err)
	require.True(t, deleted)
	_, ok = s.Snapshot().Clients["gone"]
	require.False(t, ok)
}

func TestDeleteAndSaveCommittedSyncFailureDoesNotRestoreClient(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, time.Hour)
	require.NoError(t, err)
	defer s.Close()

	s.Upsert(ClientState{ID: "gone", Name: "old"})
	oldSyncDir := syncDirFn
	syncDirFn = func(string) error { return errors.New("sync failed") }
	defer func() { syncDirFn = oldSyncDir }()

	deleted, err := s.DeleteAndSave("gone")
	require.True(t, deleted)
	require.ErrorIs(t, err, ErrCommitted)
	_, ok := s.Snapshot().Clients["gone"]
	require.False(t, ok, "a committed rename must not be rolled back in memory")

	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.NotContains(t, string(data), `"gone"`)
}

func TestExposureStatePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, time.Hour)
	require.NoError(t, err)
	s.Upsert(ClientState{
		ID:       "c1",
		Name:     "home",
		Mappings: []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222}},
	})

	require.Equal(t, "", s.Exposure("c1", "m1"), "an absent exposure is the public default")
	require.Equal(t, "", s.Exposure("missing", "m1"))
	require.NoError(t, s.SetExposure("c1", "m1", "local"))
	require.Equal(t, "local", s.Exposure("c1", "m1"))

	snapshot := s.Snapshot()
	snapshot.Clients["c1"].Exposures["m1"] = "public"
	require.Equal(t, "local", s.Exposure("c1", "m1"), "Snapshot must deep-copy exposures")

	// An ordinary client/mapping upsert with no exposure map must not erase
	// the exposure stored for this client.
	s.Upsert(ClientState{
		ID:       "c1",
		Name:     "renamed",
		Mappings: []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222}},
	})
	require.Equal(t, "local", s.Exposure("c1", "m1"))

	// A second client without exposures must not gain an "exposures" key.
	s.Upsert(ClientState{ID: "c2", Name: "plain"})
	require.NoError(t, s.Close())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var persisted State
	require.NoError(t, json.Unmarshal(data, &persisted))
	require.Nil(t, persisted.Clients["c2"].Exposures, "an absent exposure map must stay omitted")
	require.NotContains(t, string(data), `"exposures": {}`)

	reopened, err := Open(path, time.Hour)
	require.NoError(t, err)
	require.Equal(t, "local", reopened.Exposure("c1", "m1"), "exposure must survive a restart")
	require.Equal(t, "", reopened.Exposure("c1", "m2"))
	require.Equal(t, "", reopened.Exposure("c2", "m1"))

	// Explicit public is also persisted rather than normalized away.
	require.NoError(t, reopened.SetExposure("c1", "m2", "public"))
	require.NoError(t, reopened.Close())

	reopenedAgain, err := Open(path, time.Hour)
	require.NoError(t, err)
	defer reopenedAgain.Close()
	require.Equal(t, "public", reopenedAgain.Exposure("c1", "m2"))
	require.Equal(t, "local", reopenedAgain.Exposure("c1", "m1"))
}

func TestExposureDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path, time.Hour)
	require.NoError(t, err)
	s.Upsert(ClientState{
		ID:       "c1",
		Mappings: []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222}, {ID: "m2", Type: "udp", RemotePort: 2223}},
	})
	require.NoError(t, s.SetExposure("c1", "m1", "local"))
	require.NoError(t, s.SetExposure("c1", "m2", "local"))

	require.NoError(t, s.DeleteExposure("c1", "m1"))
	require.Equal(t, "", s.Exposure("c1", "m1"))
	require.Equal(t, "local", s.Exposure("c1", "m2"), "deleting one exposure must not clear another")
	require.NoError(t, s.DeleteExposure("c1", "absent"), "deleting an absent exposure is a no-op")
	require.NoError(t, s.DeleteExposure("missing", "m1"), "deleting from an unknown client is a no-op")
	require.NoError(t, s.Close())

	reopened, err := Open(path, time.Hour)
	require.NoError(t, err)
	require.Equal(t, "", reopened.Exposure("c1", "m1"), "deleted exposure must stay deleted after restart")
	require.Equal(t, "local", reopened.Exposure("c1", "m2"))

	deleted, err := reopened.DeleteAndSave("c1")
	require.NoError(t, err)
	require.True(t, deleted)
	require.Equal(t, "", reopened.Exposure("c1", "m2"), "deleting the client must remove its exposures")
	require.NoError(t, reopened.Close())

	reopenedAgain, err := Open(path, time.Hour)
	require.NoError(t, err)
	defer reopenedAgain.Close()
	require.Equal(t, "", reopenedAgain.Exposure("c1", "m2"))
}

func TestExposureSaveFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "state.json")
	s, err := Open(path, time.Hour)
	require.NoError(t, err)
	s.Upsert(ClientState{ID: "c1", Mappings: []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222}}})
	require.Equal(t, "", s.Exposure("c1", "m1"))

	// A failure before the rename is not committed: the in-memory value must
	// roll back so callers never observe state that is absent from disk.
	require.Error(t, s.SetExposure("c1", "m1", "local"))
	require.Equal(t, "", s.Exposure("c1", "m1"))

	// The same client remains usable once persistence can succeed.
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, s.SetExposure("c1", "m1", "local"))
	require.Equal(t, "local", s.Exposure("c1", "m1"))

	// Delete failures roll back the same way.
	require.NoError(t, os.RemoveAll(filepath.Dir(path)))
	require.Error(t, s.DeleteExposure("c1", "m1"))
	require.Equal(t, "local", s.Exposure("c1", "m1"))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
	require.NoError(t, s.Close())

	// A post-rename directory sync failure is reported as committed and must
	// not be rolled back: the new value is already the on-disk state.
	committedPath := filepath.Join(t.TempDir(), "state.json")
	committed, err := Open(committedPath, time.Hour)
	require.NoError(t, err)
	committed.Upsert(ClientState{ID: "c1", Mappings: []config.Mapping{{ID: "m1", Type: "tcp", RemotePort: 2222}}})
	oldSyncDir := syncDirFn
	syncDirFn = func(string) error { return errors.New("sync failed") }
	defer func() { syncDirFn = oldSyncDir }()

	err = committed.SetExposure("c1", "m1", "local")
	require.ErrorIs(t, err, ErrCommitted)
	require.Equal(t, "local", committed.Exposure("c1", "m1"), "a committed rename must not be rolled back in memory")

	data, readErr := os.ReadFile(committedPath)
	require.NoError(t, readErr)
	require.Contains(t, string(data), `"m1": "local"`)

	syncDirFn = oldSyncDir
	require.NoError(t, committed.Close())
}
