package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestStoreRoundTripsEncryptedServerAndRejectsTamper(t *testing.T) {
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
	require.Equal(t, "us", store.State().Servers[0].Name)
	require.NoError(t, store.SetServerPassword("srv_1", []byte("stored-pw")))
	require.Equal(t, 0o600, fileMode(t, statePath))
	require.Equal(t, 0o600, fileMode(t, keyPath))

	// An unrelated store must reject mutations for a record it does not know.
	otherStatePath := filepath.Join(dir, "other.json")
	otherKeyPath := filepath.Join(dir, "other.key")
	other, err := OpenStore(otherStatePath, otherKeyPath)
	require.NoError(t, err)
	require.ErrorContains(t, other.SetServerPassword("srv_1", []byte("pw")), "unknown server")

	// The same state paired with the unrelated key must fail at startup.
	stateData, err := os.ReadFile(statePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(otherStatePath, stateData, 0o600))
	_, err = OpenStore(otherStatePath, otherKeyPath)
	require.ErrorContains(t, err, "key check failed")
}

func TestStoreStateReturnsDeepCopy(t *testing.T) {
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

	snapshot := store.State()
	snapshot.Admin.Username = "mutated"
	snapshot.Servers[0].Name = "mutated"
	snapshot.Servers = append(snapshot.Servers, ServerRecord{ID: "srv_2", Name: "eu"})

	current := store.State()
	require.Equal(t, "admin", current.Admin.Username)
	require.Len(t, current.Servers, 1)
	require.Equal(t, "us", current.Servers[0].Name)
}

func TestCloneStatePreservesEmptyNonNilServers(t *testing.T) {
	cloned := cloneState(State{Servers: []ServerRecord{}})
	require.NotNil(t, cloned.Servers)
	require.Empty(t, cloned.Servers)
}

func TestUpdatePublishesStateWhenDirectorySyncFailsAfterRename(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	keyPath := filepath.Join(dir, "manager.key")
	store, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)

	originalSyncDir := syncDirFn
	syncDirFn = func(string) error { return errors.New("injected directory sync failure") }
	t.Cleanup(func() { syncDirFn = originalSyncDir })

	err = store.Update(func(s *State) error {
		s.Servers = append(s.Servers, ServerRecord{
			ID: "srv_1", Name: "us", BaseURL: "https://us.example/hy2tund",
			UpstreamUsername: "admin",
		})
		return nil
	})
	require.ErrorIs(t, err, ErrStateCommitted)
	require.Len(t, store.State().Servers, 1, "a successful rename must publish the new state")
	require.Equal(t, "srv_1", store.State().Servers[0].ID)

	onDisk, err := os.ReadFile(statePath)
	require.NoError(t, err)
	var diskState State
	require.NoError(t, json.Unmarshal(onDisk, &diskState))
	require.Len(t, diskState.Servers, 1)

	// A later successful update must build on the committed state, not the
	// pre-failure in-memory snapshot.
	syncDirFn = originalSyncDir
	require.NoError(t, store.Update(func(s *State) error {
		s.Servers = append(s.Servers, ServerRecord{
			ID: "srv_2", Name: "eu", BaseURL: "https://eu.example/hy2tund",
			UpstreamUsername: "admin",
		})
		return nil
	}))
	require.Len(t, store.State().Servers, 2)

	reopened, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)
	require.Len(t, reopened.State().Servers, 2)
}

func TestOpenStoreCreatesAdminAndPrintsPasswordOnce(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	keyPath := filepath.Join(dir, "manager.key")

	t.Setenv(InitialAdminPasswordEnv, "")
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

func TestStateFileContainsNeitherPlaintextPasswordNorKey(t *testing.T) {
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
	const plaintext = "unique-upstream-password"
	require.NoError(t, store.SetServerPassword("srv_1", []byte(plaintext)))

	stateData, err := os.ReadFile(statePath)
	require.NoError(t, err)
	keyData, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	require.Contains(t, string(stateData), "upstream_password_enc")
	require.NotContains(t, string(stateData), plaintext)
	require.NotContains(t, string(stateData), strings.TrimSpace(string(keyData)))
}

func TestStoreRejectsTamperedCiphertextOnLoad(t *testing.T) {
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

	data, err := os.ReadFile(statePath)
	require.NoError(t, err)
	var state State
	require.NoError(t, json.Unmarshal(data, &state))
	state.Servers[0].UpstreamPasswordEnc = flipLastHexByte(t, state.Servers[0].UpstreamPasswordEnc)
	tampered, err := json.MarshalIndent(state, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, append(tampered, '\n'), 0o600))

	_, err = OpenStore(statePath, keyPath)
	require.ErrorContains(t, err, "cannot decrypt upstream password")
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
	require.ErrorContains(t, err, "missing while state")

	// A different key file must be rejected at startup.
	otherDir := t.TempDir()
	otherStatePath := filepath.Join(otherDir, "state.json")
	otherKeyPath := filepath.Join(otherDir, "manager.key")
	_, err = OpenStore(otherStatePath, otherKeyPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(otherStatePath, valid, 0o600))
	_, err = OpenStore(otherStatePath, otherKeyPath)
	require.ErrorContains(t, err, "key check failed")
}

func TestOpenStoreFailsClosedWhenStateMissingButKeyExists(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	keyPath := filepath.Join(dir, "manager.key")

	_, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)
	require.NoError(t, os.Remove(statePath))

	_, err = OpenStore(statePath, keyPath)
	require.ErrorContains(t, err, "missing while manager key")
	require.NoFileExists(t, statePath)
}

func TestOpenStoreKeyCheckDetectsWrongKeyWithoutServerPasswords(t *testing.T) {
	dir := t.TempDir()
	firstDir := filepath.Join(dir, "first")
	secondDir := filepath.Join(dir, "second")
	require.NoError(t, os.MkdirAll(firstDir, 0o700))
	require.NoError(t, os.MkdirAll(secondDir, 0o700))

	firstStatePath := filepath.Join(firstDir, "state.json")
	firstKeyPath := filepath.Join(firstDir, "manager.key")
	_, err := OpenStore(firstStatePath, firstKeyPath)
	require.NoError(t, err)

	secondStatePath := filepath.Join(secondDir, "state.json")
	secondKeyPath := filepath.Join(secondDir, "manager.key")
	_, err = OpenStore(secondStatePath, secondKeyPath)
	require.NoError(t, err)

	firstState, err := os.ReadFile(firstStatePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(secondStatePath, firstState, 0o600))

	_, err = OpenStore(secondStatePath, secondKeyPath)
	require.ErrorContains(t, err, "key check failed")
}

func TestOpenStoreCreatesParentDirectories(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state", "nested", "state.json")
	keyPath := filepath.Join(dir, "keys", "nested", "manager.key")

	store, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)
	require.FileExists(t, statePath)
	require.FileExists(t, keyPath)
	require.NotEmpty(t, store.State().Admin.PasswordHash)
	require.Equal(t, 0o700, fileMode(t, filepath.Dir(statePath)))
	require.Equal(t, 0o700, fileMode(t, filepath.Dir(keyPath)))
}

func TestOpenStoreRemovesOrphanKeyAfterFirstStateSaveFailure(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	keyPath := filepath.Join(dir, "manager.key")

	originalWrite := atomicWriteFn
	atomicWriteFn = func(string, []byte) (bool, error) {
		return false, errors.New("injected first state save failure")
	}
	t.Cleanup(func() { atomicWriteFn = originalWrite })

	_, err := OpenStore(statePath, keyPath)
	require.ErrorContains(t, err, "initialize manager state")
	require.NoFileExists(t, statePath)
	require.NoFileExists(t, keyPath, "a failed first state save must not leave an orphan key")

	atomicWriteFn = originalWrite
	store, err := OpenStore(statePath, keyPath)
	require.NoError(t, err, "a clean retry after orphan-key cleanup must succeed")
	require.NotEmpty(t, store.State().Admin.PasswordHash)
}

func TestOpenStoreMigratesMissingKeyCheck(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	keyPath := filepath.Join(dir, "manager.key")

	store, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)
	require.NotEmpty(t, store.State().KeyCheck)

	data, err := os.ReadFile(statePath)
	require.NoError(t, err)
	var state State
	require.NoError(t, json.Unmarshal(data, &state))
	state.KeyCheck = ""
	encoded, err := json.MarshalIndent(state, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, append(encoded, '\n'), 0o600))

	reopened, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)
	require.NotEmpty(t, reopened.State().KeyCheck, "a legacy state must gain a key check")

	otherDir := t.TempDir()
	_, otherKey, err := func() (*Store, []byte, error) {
		other, openErr := OpenStore(filepath.Join(otherDir, "state.json"), filepath.Join(otherDir, "manager.key"))
		if openErr != nil {
			return nil, nil, openErr
		}
		keyData, readErr := os.ReadFile(filepath.Join(otherDir, "manager.key"))
		return other, keyData, readErr
	}()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keyPath, otherKey, 0o600))

	_, err = OpenStore(statePath, keyPath)
	require.ErrorContains(t, err, "key check failed")
}

func TestOpenStoreInitialPasswordFromEnvOnlyOnFirstRun(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	keyPath := filepath.Join(dir, "manager.key")
	const envPassword = "env-initial-admin-password"
	t.Setenv(InitialAdminPasswordEnv, envPassword)

	oldWriter := initialAdminPasswordWriter
	var output bytes.Buffer
	initialAdminPasswordWriter = &output
	t.Cleanup(func() { initialAdminPasswordWriter = oldWriter })

	store, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)
	state := store.State()
	require.NoError(t, bcrypt.CompareHashAndPassword([]byte(state.Admin.PasswordHash), []byte(envPassword)))
	require.Empty(t, output.String(), "an environment-supplied initial password must not be printed")

	t.Setenv(InitialAdminPasswordEnv, "a-different-password")
	reopened, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)
	require.Equal(t, state.Admin.PasswordHash, reopened.State().Admin.PasswordHash,
		"the environment variable must never reset an existing administrator")
}

func TestStateFileTightensPermissionsOnLoad(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	keyPath := filepath.Join(dir, "manager.key")

	_, err := OpenStore(statePath, keyPath)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(statePath, 0o644))
	require.NoError(t, os.Chmod(keyPath, 0o644))

	_, err = OpenStore(statePath, keyPath)
	require.NoError(t, err)
	require.Equal(t, 0o600, fileMode(t, statePath))
	require.Equal(t, 0o600, fileMode(t, keyPath))
}

func fileMode(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return int(info.Mode().Perm())
}
