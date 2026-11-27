package manager

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestStoreRoundTripsEncryptedServerAndRejectsTamper(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.json"), filepath.Join(dir, "manager.key"))
	require.NoError(t, err)
	require.NoError(t, store.Update(func(s *State) error {
		s.Servers = append(s.Servers, ServerRecord{
			ID: "srv_1", Name: "us", BaseURL: "https://us.example/hy2tund",
			UpstreamUsername: "admin",
		})
		return nil
	}))
	require.Equal(t, "us", store.State().Servers[0].Name)
	require.Equal(t, 0o600, fileMode(t, filepath.Join(dir, "state.json")))
	require.Equal(t, 0o600, fileMode(t, filepath.Join(dir, "manager.key")))

	// A different key must not decrypt existing records.
	other, err := OpenStore(filepath.Join(dir, "other.json"), filepath.Join(dir, "other.key"))
	require.NoError(t, err)
	require.ErrorContains(t, other.SetServerPassword("srv_1", []byte("pw")), "unknown server")
}

func TestOpenStoreCreatesAdminAndPrintsPasswordOnce(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	keyPath := filepath.Join(dir, "manager.key")

	oldGenerator := initialAdminPasswordGenerator
	oldWriter := initialAdminPasswordWriter
	var output bytes.Buffer
	initialAdminPasswordGenerator = func() (string, error) { return "test-initial-password", nil }
	initialAdminPasswordWriter = &output
	t.Cleanup(func() {
		initialAdminPasswordGenerator = oldGenerator
		initialAdminPasswordWriter = oldWriter
	})

	store, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)
	require.Contains(t, output.String(), "initial admin password: test-initial-password")

	state := store.State()
	require.Equal(t, "admin", state.Admin.Username)
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(state.Admin.PasswordHash), []byte("test-initial-password")))

	output.Reset()
	reopened, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)
	require.Empty(t, output.String(), "an existing administrator must never be reinitialized")
	require.Equal(t, state.Admin.PasswordHash, reopened.State().Admin.PasswordHash)
}

func TestStorePasswordRoundTripPersistsCiphertext(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	keyPath := filepath.Join(dir, "manager.key")

	store, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)
	require.NoError(t, store.Update(func(s *State) error {
		s.Servers = append(s.Servers, ServerRecord{
			ID: "srv_1", Name: "us", BaseURL: "https://us.example/hy2tund",
			UpstreamUsername: "admin",
			CreatedAt:        time.Now().UTC(),
		})
		return nil
	}))
	require.NoError(t, store.SetServerPassword("srv_1", []byte("upstream-pw")))

	record := store.State().Servers[0]
	require.NotEmpty(t, record.UpstreamPasswordEnc)
	require.NotContains(t, record.UpstreamPasswordEnc, "upstream-pw")
	require.False(t, record.UpdatedAt.IsZero())

	password, err := store.ServerPassword("srv_1")
	require.NoError(t, err)
	require.Equal(t, "upstream-pw", string(password))

	reopened, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)
	password, err = reopened.ServerPassword("srv_1")
	require.NoError(t, err)
	require.Equal(t, "upstream-pw", string(password))

	_, err = reopened.ServerPassword("srv_missing")
	require.ErrorContains(t, err, "unknown server")
}

func TestOpenStoreFailsClosedOnCorruptStateAndMissingKey(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	keyPath := filepath.Join(dir, "manager.key")

	store, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)
	require.NoError(t, store.Update(func(s *State) error {
		s.Servers = append(s.Servers, ServerRecord{
			ID: "srv_1", Name: "us", BaseURL: "https://us.example/hy2tund",
			UpstreamUsername: "admin",
		})
		return nil
	}))
	require.NoError(t, store.SetServerPassword("srv_1", []byte("upstream-pw")))

	valid, err := os.ReadFile(statePath)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(statePath, []byte("{"), 0o600))
	_, err = OpenStore(statePath, keyPath)
	require.ErrorContains(t, err, "parse manager state")

	require.NoError(t, os.WriteFile(statePath, valid, 0o600))
	require.NoError(t, os.Remove(keyPath))
	_, err = OpenStore(statePath, keyPath)
	require.ErrorContains(t, err, "missing while state exists")

	// A different key file must make the existing ciphertext undecryptable.
	otherDir := t.TempDir()
	otherStatePath := filepath.Join(otherDir, "state.json")
	otherKeyPath := filepath.Join(otherDir, "manager.key")
	_, err = OpenStore(otherStatePath, otherKeyPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(otherStatePath, valid, 0o600))
	_, err = OpenStore(otherStatePath, otherKeyPath)
	require.ErrorContains(t, err, "cannot decrypt upstream password")
}

func fileMode(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return int(info.Mode().Perm())
}
