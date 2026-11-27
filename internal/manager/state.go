package manager

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

const (
	adminUsername               = "admin"
	defaultInitialAdminPassword = "admin"
	managerKeyFileMode          = 0o600
	managerStateFileMode        = 0o600
	managerStateFileSuffix      = ".tmp-"
	managerKeyCheckValue        = "hy2tun-manager-key-check"

	// InitialAdminPasswordEnv supplies the first-run administrator password.
	// It is ignored after state.json exists, so an existing administrator can
	// never be reset by leaving the variable in the service environment.
	InitialAdminPasswordEnv = "HY2TUN_MANAGER_INITIAL_PASSWORD"

	minAdminPasswordRunes = 8
	maxAdminPasswordBytes = 72
)

// ErrStateCommitted reports that the state file rename succeeded but the
// directory fsync failed. The new state is authoritative and has been
// published in memory, but its survival across a machine crash is not
// guaranteed.
var ErrStateCommitted = errors.New("manager state committed but directory sync failed")

// Package-level seam lets tests capture the one-time output without touching
// the production defaults.
var (
	// initialAdminPasswordWriter defaults to os.Stderr at call time so tests
	// can redirect the process stderr or supply their own buffer.
	initialAdminPasswordWriter io.Writer
)

// AdminRecord is the single manager administrator account.
type AdminRecord struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

// ServerRecord is one upstream hy2tund server in the manager inventory.
type ServerRecord struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	BaseURL             string    `json:"base_url"`
	UpstreamUsername    string    `json:"upstream_username"`
	UpstreamPasswordEnc string    `json:"upstream_password_enc,omitempty"`
	TLSSkipVerify       bool      `json:"tls_skip_verify"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// State is the full JSON document persisted in the manager state file.
// KeyCheck is an encrypted sentinel that detects a mismatched manager key even
// when no server passwords have been stored yet.
type State struct {
	Admin    AdminRecord    `json:"admin"`
	Servers  []ServerRecord `json:"servers"`
	KeyCheck string         `json:"key_check,omitempty"`
}

// Store owns the manager state and encryption key. All mutations go through
// Update so the on-disk file is replaced atomically under a mutex.
type Store struct {
	mu        sync.RWMutex
	statePath string
	key       []byte
	state     State
}

// OpenStore loads or creates the manager state and encryption key. State and
// key are a matched pair: if exactly one of them exists, startup fails closed
// rather than silently replacing a key or generating a fresh administrator.
// Both missing means first run; both present means normal startup.
func OpenStore(statePath, keyPath string) (*Store, error) {
	if strings.TrimSpace(statePath) == "" {
		return nil, fmt.Errorf("manager state path is empty")
	}
	if strings.TrimSpace(keyPath) == "" {
		return nil, fmt.Errorf("manager key path is empty")
	}
	if err := ensureParentDir(statePath); err != nil {
		return nil, fmt.Errorf("create manager state directory for %s: %w", statePath, err)
	}
	if err := ensureParentDir(keyPath); err != nil {
		return nil, fmt.Errorf("create manager key directory for %s: %w", keyPath, err)
	}

	stateExists, err := pathExists(statePath)
	if err != nil {
		return nil, fmt.Errorf("inspect manager state %s: %w", statePath, err)
	}
	keyExists, err := pathExists(keyPath)
	if err != nil {
		return nil, fmt.Errorf("inspect manager key %s: %w", keyPath, err)
	}
	switch {
	case stateExists && !keyExists:
		return nil, fmt.Errorf("manager key %s is missing while state %s exists; restore manager.key and state.json together (refusing to replace the encryption key)", keyPath, statePath)
	case !stateExists && keyExists:
		return nil, fmt.Errorf("manager state %s is missing while manager key %s exists; restore state.json (refusing to create a fresh administrator)", statePath, keyPath)
	}

	key, keyCreated, err := loadOrCreateManagerKey(keyPath)
	if err != nil {
		if keyCreated {
			_ = os.Remove(keyPath)
			_ = syncDirFn(filepath.Dir(keyPath))
		}
		return nil, err
	}

	store := &Store{statePath: statePath, key: key}
	if !stateExists {
		password, fromEnv, err := initialAdminPassword()
		if err != nil {
			if keyCreated {
				_ = os.Remove(keyPath)
				_ = syncDirFn(filepath.Dir(keyPath))
			}
			return nil, err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			if keyCreated {
				_ = os.Remove(keyPath)
				_ = syncDirFn(filepath.Dir(keyPath))
			}
			return nil, fmt.Errorf("hash initial admin password: %w", err)
		}
		keyCheck, err := EncryptSecret(key, []byte(managerKeyCheckValue))
		if err != nil {
			if keyCreated {
				_ = os.Remove(keyPath)
				_ = syncDirFn(filepath.Dir(keyPath))
			}
			return nil, fmt.Errorf("create manager key check: %w", err)
		}
		if !fromEnv {
			passwordWriter := initialAdminPasswordWriter
			if passwordWriter == nil {
				passwordWriter = os.Stderr
			}
			if _, err := fmt.Fprintf(passwordWriter, "hy2tun-manager: initial admin password: %s\n", password); err != nil {
				if keyCreated {
					_ = os.Remove(keyPath)
					_ = syncDirFn(filepath.Dir(keyPath))
				}
				return nil, fmt.Errorf("print initial admin password: %w", err)
			}
		}
		store.state = State{
			Admin:    AdminRecord{Username: adminUsername, PasswordHash: string(hash)},
			Servers:  []ServerRecord{},
			KeyCheck: keyCheck,
		}
		if _, err := store.saveLocked(store.state); err != nil {
			// A failed first state save must not leave a lone manager.key:
			// the next start intentionally refuses a key without state, which
			// would wedge recovery. Keep the key only if the state rename did
			// commit (ErrStateCommitted) and the state file now exists.
			if keyCreated {
				if exists, statErr := pathExists(statePath); statErr == nil && !exists {
					_ = os.Remove(keyPath)
					_ = syncDirFn(filepath.Dir(keyPath))
				}
			}
			return nil, fmt.Errorf("initialize manager state %s: %w", statePath, err)
		}
		return store, nil
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("read manager state %s: %w", statePath, err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse manager state %s: %w", statePath, err)
	}
	if err := validateState(state); err != nil {
		return nil, fmt.Errorf("invalid manager state %s: %w", statePath, err)
	}
	if state.KeyCheck != "" {
		check, err := DecryptSecret(key, state.KeyCheck)
		if err != nil || string(check) != managerKeyCheckValue {
			return nil, fmt.Errorf("manager state %s: key check failed; manager.key does not match this state (restore state.json and manager.key together)", statePath)
		}
	}
	for _, server := range state.Servers {
		if server.UpstreamPasswordEnc == "" {
			continue
		}
		if _, err := DecryptSecret(key, server.UpstreamPasswordEnc); err != nil {
			return nil, fmt.Errorf("manager state %s: cannot decrypt upstream password for server %q: %w (restore state.json and manager.key together)", statePath, server.ID, err)
		}
	}
	if err := ensureFileMode(statePath, managerStateFileMode); err != nil {
		return nil, fmt.Errorf("secure manager state %s: %w", statePath, err)
	}
	store.state = state
	if state.KeyCheck == "" {
		// Migrate states written before key_check existed. Persisting the
		// sentinel now lets a later wrong-key startup fail even when no server
		// password ciphertext exists yet.
		store.state.KeyCheck, err = EncryptSecret(key, []byte(managerKeyCheckValue))
		if err != nil {
			return nil, fmt.Errorf("create manager key check for %s: %w", statePath, err)
		}
		if _, err := store.saveLocked(store.state); err != nil {
			return nil, fmt.Errorf("migrate manager key check in %s: %w", statePath, err)
		}
	}
	return store, nil
}

// initialAdminPassword returns the first-run administrator password. The
// initial-password environment variable is consulted only while state.json is
// missing; without it the fixed bootstrap password is used and printed once by
// the caller. Environment-supplied passwords are validated before hashing.
func initialAdminPassword() (password string, fromEnv bool, err error) {
	if value, ok := os.LookupEnv(InitialAdminPasswordEnv); ok && value != "" {
		if utf8.RuneCountInString(value) < minAdminPasswordRunes {
			return "", false, fmt.Errorf("initial admin password from %s must be at least %d characters", InitialAdminPasswordEnv, minAdminPasswordRunes)
		}
		if len(value) > maxAdminPasswordBytes {
			return "", false, fmt.Errorf("initial admin password from %s must not exceed %d bytes", InitialAdminPasswordEnv, maxAdminPasswordBytes)
		}
		return value, true, nil
	}
	return defaultInitialAdminPassword, false, nil
}

// ensureParentDir creates the parent directory of path when it does not exist.
// The manager owns its state directory, so 0700 is the safe default; existing
// directories are left untouched.
func ensureParentDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o700)
}

// State returns a deep-safe copy of the current state for reads.
func (s *Store) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneState(s.state)
}

// SetAdminPassword hashes a new manager administrator password at the current
// bcrypt cost and persists it. The plaintext is never logged or returned. The
// manager's HTTP shell snapshots the hash at construction, so a running manager
// must be restarted before the new password is accepted.
func (s *Store) SetAdminPassword(password string) error {
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("administrator password is required")
	}
	if len(password) > maxAdminPasswordBytes {
		return fmt.Errorf("administrator password must not exceed %d bytes", maxAdminPasswordBytes)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash administrator password: %w", err)
	}
	return s.Update(func(state *State) error {
		state.Admin.PasswordHash = string(hash)
		return nil
	})
}

// Update mutates a copy of the state and atomically persists it. If the
// mutation or the rename fails, the in-memory state is left unchanged. When
// the rename succeeds but the directory fsync fails, the new state is
// published and ErrStateCommitted is returned: disk and memory agree, but the
// commit's crash durability is uncertain.
func (s *Store) Update(mutate func(*State) error) error {
	if mutate == nil {
		return fmt.Errorf("manager state update function is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	next := cloneState(s.state)
	if err := mutate(&next); err != nil {
		return err
	}
	if err := validateState(next); err != nil {
		return fmt.Errorf("manager state: %w", err)
	}
	committed, err := s.saveLocked(next)
	if committed {
		s.state = next
	}
	if err != nil {
		return err
	}
	return nil
}

// SetServerPassword encrypts an upstream password and persists it for the
// server with the given ID.
func (s *Store) SetServerPassword(id string, plaintext []byte) error {
	s.mu.RLock()
	key := append([]byte(nil), s.key...)
	s.mu.RUnlock()

	encoded, err := EncryptSecret(key, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt upstream password: %w", err)
	}
	return s.Update(func(state *State) error {
		for i := range state.Servers {
			if state.Servers[i].ID != id {
				continue
			}
			state.Servers[i].UpstreamPasswordEnc = encoded
			state.Servers[i].UpdatedAt = time.Now().UTC()
			return nil
		}
		return fmt.Errorf("unknown server %q", id)
	})
}

// AddServer encrypts the upstream password and appends the record in the same
// state update, so a server is never persisted without a usable credential.
func (s *Store) AddServer(record ServerRecord, password []byte) error {
	if len(password) == 0 {
		return fmt.Errorf("upstream password is empty")
	}
	s.mu.RLock()
	key := append([]byte(nil), s.key...)
	s.mu.RUnlock()

	encoded, err := EncryptSecret(key, password)
	if err != nil {
		return fmt.Errorf("encrypt upstream password: %w", err)
	}
	record.UpstreamPasswordEnc = encoded
	return s.Update(func(state *State) error {
		for _, existing := range state.Servers {
			if existing.ID == record.ID {
				return fmt.Errorf("duplicate server id %q", record.ID)
			}
			if strings.EqualFold(strings.TrimSpace(existing.Name), strings.TrimSpace(record.Name)) {
				return fmt.Errorf("duplicate server name %q", record.Name)
			}
		}
		state.Servers = append(state.Servers, record)
		return nil
	})
}

// UpdateServer replaces the editable fields of an existing record. A nil
// password keeps the stored ciphertext; a non-nil password is encrypted and
// replaces it. CreatedAt and the server ID are immutable.
func (s *Store) UpdateServer(record ServerRecord, password []byte) error {
	var encoded string
	if password != nil {
		s.mu.RLock()
		key := append([]byte(nil), s.key...)
		s.mu.RUnlock()
		var err error
		encoded, err = EncryptSecret(key, password)
		if err != nil {
			return fmt.Errorf("encrypt upstream password: %w", err)
		}
	}
	return s.Update(func(state *State) error {
		for i := range state.Servers {
			existing := &state.Servers[i]
			if existing.ID != record.ID {
				continue
			}
			for j := range state.Servers {
				if j != i && strings.EqualFold(strings.TrimSpace(state.Servers[j].Name), strings.TrimSpace(record.Name)) {
					return fmt.Errorf("duplicate server name %q", record.Name)
				}
			}
			existing.Name = record.Name
			existing.BaseURL = record.BaseURL
			existing.UpstreamUsername = record.UpstreamUsername
			existing.TLSSkipVerify = record.TLSSkipVerify
			existing.UpdatedAt = time.Now().UTC()
			if password != nil {
				existing.UpstreamPasswordEnc = encoded
			}
			return nil
		}
		return fmt.Errorf("unknown server %q", record.ID)
	})
}

// RemoveServer removes one inventory record and returns the removed value. It
// never contacts the upstream server.
func (s *Store) RemoveServer(id string) (ServerRecord, error) {
	var removed ServerRecord
	err := s.Update(func(state *State) error {
		for i := range state.Servers {
			if state.Servers[i].ID != id {
				continue
			}
			removed = state.Servers[i]
			state.Servers = append(state.Servers[:i], state.Servers[i+1:]...)
			return nil
		}
		return fmt.Errorf("unknown server %q", id)
	})
	if err != nil {
		return ServerRecord{}, err
	}
	return removed, nil
}

// ServerPassword decrypts a server's upstream password for an in-memory
// upstream session. Callers must never render or log the returned bytes.
func (s *Store) ServerPassword(id string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, server := range s.state.Servers {
		if server.ID != id {
			continue
		}
		if server.UpstreamPasswordEnc == "" {
			return nil, fmt.Errorf("server %q has no stored password", id)
		}
		plaintext, err := DecryptSecret(s.key, server.UpstreamPasswordEnc)
		if err != nil {
			return nil, fmt.Errorf("decrypt upstream password for server %q: %w", id, err)
		}
		return plaintext, nil
	}
	return nil, fmt.Errorf("unknown server %q", id)
}

func cloneState(state State) State {
	cloned := state
	if state.Servers != nil {
		cloned.Servers = make([]ServerRecord, len(state.Servers))
		copy(cloned.Servers, state.Servers)
	}
	return cloned
}

func validateState(state State) error {
	if strings.TrimSpace(state.Admin.Username) == "" {
		return fmt.Errorf("admin.username must not be empty")
	}
	if state.Admin.PasswordHash == "" {
		return fmt.Errorf("admin.password_hash must not be empty")
	}
	if _, err := bcrypt.Cost([]byte(state.Admin.PasswordHash)); err != nil {
		return fmt.Errorf("admin.password_hash is not a valid bcrypt hash: %w", err)
	}

	ids := make(map[string]struct{}, len(state.Servers))
	names := make(map[string]struct{}, len(state.Servers))
	for i, server := range state.Servers {
		if strings.TrimSpace(server.ID) == "" {
			return fmt.Errorf("servers[%d].id must not be empty", i)
		}
		if _, exists := ids[server.ID]; exists {
			return fmt.Errorf("duplicate server id %q", server.ID)
		}
		ids[server.ID] = struct{}{}
		if strings.TrimSpace(server.Name) == "" {
			return fmt.Errorf("servers[%d].name must not be empty", i)
		}
		nameKey := strings.ToLower(strings.TrimSpace(server.Name))
		if _, exists := names[nameKey]; exists {
			return fmt.Errorf("duplicate server name %q", server.Name)
		}
		names[nameKey] = struct{}{}
		if strings.TrimSpace(server.BaseURL) == "" {
			return fmt.Errorf("servers[%d].base_url must not be empty", i)
		}
		if strings.TrimSpace(server.UpstreamUsername) == "" {
			return fmt.Errorf("servers[%d].upstream_username must not be empty", i)
		}
	}
	return nil
}

func (s *Store) saveLocked(state State) (bool, error) {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode manager state: %w", err)
	}
	data = append(data, '\n')
	committed, err := atomicWriteFn(s.statePath, data)
	if err != nil {
		return committed, fmt.Errorf("write manager state %s: %w", s.statePath, err)
	}
	return true, nil
}

// loadOrCreateManagerKey reads a 32-byte hex-encoded key, creating it with
// mode 0600 on first use. Callers only reach creation when both state.json and
// manager.key are absent; OpenStore rejects a lone key or lone state. created
// reports that this call installed the key, so OpenStore can remove it when
// the matching first state save fails before publishing state.json.
func loadOrCreateManagerKey(path string) (key []byte, created bool, err error) {
	data, err := os.ReadFile(path)
	if err == nil {
		key, err := decodeManagerKey(data)
		if err != nil {
			return nil, false, fmt.Errorf("load manager key %s: %w", path, err)
		}
		if err := ensureFileMode(path, managerKeyFileMode); err != nil {
			return nil, false, fmt.Errorf("secure manager key %s: %w", path, err)
		}
		return key, false, nil
	}
	if !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("read manager key %s: %w", path, err)
	}

	key = make([]byte, secretKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, false, fmt.Errorf("generate manager key: %w", err)
	}
	encoded := []byte(hex.EncodeToString(key) + "\n")

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, managerKeyFileMode)
	if err != nil {
		if os.IsExist(err) {
			// Another process won the creation race; read its key.
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil, false, fmt.Errorf("read manager key %s after concurrent create: %w", path, readErr)
			}
			key, decodeErr := decodeManagerKey(data)
			if decodeErr != nil {
				return nil, false, decodeErr
			}
			return key, false, nil
		}
		return nil, false, fmt.Errorf("create manager key %s: %w", path, err)
	}
	if err := file.Chmod(managerKeyFileMode); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, false, fmt.Errorf("secure manager key %s: %w", path, err)
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, false, fmt.Errorf("write manager key %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, false, fmt.Errorf("sync manager key %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, false, fmt.Errorf("close manager key %s: %w", path, err)
	}
	if err := syncDirFn(filepath.Dir(path)); err != nil {
		return nil, true, fmt.Errorf("sync manager key directory: %w", err)
	}
	return key, true, nil
}

func decodeManagerKey(data []byte) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("key is not valid hex: %w", err)
	}
	if len(key) != secretKeySize {
		return nil, fmt.Errorf("key must be %d bytes, got %d", secretKeySize, len(key))
	}
	return key, nil
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory, fsyncs it, and renames it into place with mode 0600. The rename
// is the commit point: the returned committed flag is true as soon as the
// rename succeeds, even if the following directory fsync fails.
func writeFileAtomic(path string, data []byte) (committed bool, err error) {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+managerStateFileSuffix)
	if err != nil {
		return false, err
	}
	tmpName := file.Name()
	defer os.Remove(tmpName)

	if err := file.Chmod(managerStateFileMode); err != nil {
		_ = file.Close()
		return false, err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, err
	}
	if err := syncDirFn(dir); err != nil {
		return true, fmt.Errorf("%w: %v", ErrStateCommitted, err)
	}
	return true, nil
}

// syncDirFn is replaceable so tests can inject directory-sync failures.
var syncDirFn = syncDir

// atomicWriteFn is replaceable so tests can inject a first-save failure
// before the state rename commits.
var atomicWriteFn = writeFileAtomic

// syncDir flushes a directory entry so a rename survives a crash. Windows
// does not support syncing directory handles; the rename is still atomic.
func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		if runtime.GOOS == "windows" {
			return nil
		}
		return err
	}
	return nil
}

// pathExists reports whether path exists without treating any other stat error
// as absence.
func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

// ensureFileMode tightens a file to mode if it is currently broader.
func ensureFileMode(path string, mode os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() == mode {
		return nil
	}
	return os.Chmod(path, mode)
}
