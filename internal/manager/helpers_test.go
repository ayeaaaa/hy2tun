package manager

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// testStore creates a manager store with a known admin username and password.
// The initial generated password is discarded so test output stays quiet.
func testStore(t *testing.T) *Store {
	t.Helper()

	t.Setenv(InitialAdminPasswordEnv, "")
	oldWriter := initialAdminPasswordWriter
	initialAdminPasswordWriter = io.Discard
	t.Cleanup(func() { initialAdminPasswordWriter = oldWriter })

	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.json"), filepath.Join(dir, "manager.key"))
	require.NoError(t, err)

	hash, err := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	require.NoError(t, err)
	require.NoError(t, store.Update(func(state *State) error {
		state.Admin.Username = "admin"
		state.Admin.PasswordHash = string(hash)
		return nil
	}))
	return store
}

func testAudit(t *testing.T) *AuditLog {
	t.Helper()
	return NewAuditLog(filepath.Join(t.TempDir(), "audit.jsonl"))
}

func testConfig() Config {
	return Config{BasePath: "/"}
}
