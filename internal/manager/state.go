package manager

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	adminUsername          = "admin"
	adminPasswordLength    = 20
	adminPasswordAlphabet  = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	managerKeyFileMode     = 0o600
	managerStateFileMode   = 0o600
	managerStateFileSuffix = ".tmp-"
)

// Package-level seams let tests inject a known first-run password and capture
// the one-time output without touching the production defaults.
var (
	initialAdminPasswordGenerator = generateAdminPassword
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
type State struct {
	Admin   AdminRecord    `json:"admin"`
	Servers []ServerRecord `json:"servers"`
}

// Store owns the manager state and encryption key. All mutations go through
// Update so the on-disk file is replaced atomically under a mutex.
type Store struct {
	mu        sync.RWMutex
	statePath string
	key       []byte
	state     State
}

// OpenStore loads or creates the manager state and encryption key. A missing
// key is only generated when the state is also being created for the first
// time; an existing state with no key fails closed instead of silently
// replacing the encryption key.
func OpenStore(statePath, keyPath string) (*Store, error) {
	if strings.TrimSpace(statePath) == "" {
		return nil, fmt.Errorf("manager state path is empty")
	}
	if strings.TrimSpace(keyPath) == "" {
		return nil, fmt.Errorf("manager key path is empty")
	}

	stateExists := true
	if _, err := os.Stat(statePath); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect manager state %s: %w", statePath, err)
		}
		stateExists = false
	}

	key, err := loadOrCreateManagerKey(keyPath, stateExists)
	if err != nil {
		return nil, err
	}

	store := &Store{statePath: statePath, key: key}
	if !stateExists {
		password, err := initialAdminPasswordGenerator()
		if err != nil {
			return nil, fmt.Errorf("generate initial admin password: %w", err)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("hash initial admin password: %w", err)
		}
		passwordWriter := initialAdminPasswordWriter
		if passwordWriter == nil {
			passwordWriter = os.Stderr
		}
		if _, err := fmt.Fprintf(passwordWriter, "hy2tun-manager: initial admin password: %s\n", password); err != nil {
			return nil, fmt.Errorf("print initial admin password: %w", err)
		}
		store.state = State{
			Admin:   AdminRecord{Username: adminUsername, PasswordHash: string(hash)},
			Servers: []ServerRecord{},
		}
		if err := store.saveLocked(store.state); err != nil {
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
	for _, server := range state.Servers {
		if server.UpstreamPasswordEnc == "" {
			continue
		}
		if _, err := DecryptSecret(key, server.UpstreamPasswordEnc); err != nil {
			return nil, fmt.Errorf("manager state %s: cannot decrypt upstream password for server %q: %w (restore state.json and manager.key together)", statePath, server.ID, err)
		}
	}
	store.state = state
	return store, nil
}

// State returns a deep-safe copy of the current state for reads.
func (s *Store) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneState(s.state)
}

// Update mutates a copy of the state and atomically persists it. If the
// mutation or the write fails, the in-memory state is left unchanged.
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
	if err := s.saveLocked(next); err != nil {
		return err
	}
	s.state = next
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
	cloned.Servers = append([]ServerRecord(nil), state.Servers...)
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
		if _, exists := names[server.Name]; exists {
			return fmt.Errorf("duplicate server name %q", server.Name)
		}
		names[server.Name] = struct{}{}
		if strings.TrimSpace(server.BaseURL) == "" {
			return fmt.Errorf("servers[%d].base_url must not be empty", i)
		}
		if strings.TrimSpace(server.UpstreamUsername) == "" {
			return fmt.Errorf("servers[%d].upstream_username must not be empty", i)
		}
	}
	return nil
}

func (s *Store) saveLocked(state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manager state: %w", err)
	}
	data = append(data, '\n')
	if err := writeFileAtomic(s.statePath, data); err != nil {
		return fmt.Errorf("write manager state %s: %w", s.statePath, err)
	}
	return nil
}

// loadOrCreateManagerKey reads a 32-byte hex-encoded key, creating it with
// mode 0600 on first use. When state already exists, a missing key is a fatal
// error: generating a new one would make existing ciphertext unreadable.
func loadOrCreateManagerKey(path string, stateExists bool) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		key, err := decodeManagerKey(data)
		if err != nil {
			return nil, fmt.Errorf("load manager key %s: %w", path, err)
		}
		if info, statErr := os.Stat(path); statErr != nil {
			return nil, fmt.Errorf("inspect manager key %s: %w", path, statErr)
		} else if info.Mode().Perm() != managerKeyFileMode {
			if chmodErr := os.Chmod(path, managerKeyFileMode); chmodErr != nil {
				return nil, fmt.Errorf("secure manager key %s: %w", path, chmodErr)
			}
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read manager key %s: %w", path, err)
	}
	if stateExists {
		return nil, fmt.Errorf("manager key %s is missing while state exists; restore manager.key and state.json together (refusing to replace the encryption key)", path)
	}

	key := make([]byte, secretKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate manager key: %w", err)
	}
	encoded := []byte(hex.EncodeToString(key) + "\n")

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, managerKeyFileMode)
	if err != nil {
		if os.IsExist(err) {
			// Another process won the creation race; read its key.
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil, fmt.Errorf("read manager key %s after concurrent create: %w", path, readErr)
			}
			return decodeManagerKey(data)
		}
		return nil, fmt.Errorf("create manager key %s: %w", path, err)
	}
	if err := file.Chmod(managerKeyFileMode); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("secure manager key %s: %w", path, err)
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("write manager key %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("sync manager key %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("close manager key %s: %w", path, err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("sync manager key directory: %w", err)
	}
	return key, nil
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

func generateAdminPassword() (string, error) {
	limit := big.NewInt(int64(len(adminPasswordAlphabet)))
	var builder strings.Builder
	builder.Grow(adminPasswordLength)
	for i := 0; i < adminPasswordLength; i++ {
		index, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		builder.WriteByte(adminPasswordAlphabet[index.Int64()])
	}
	return builder.String(), nil
}

// writeFileAtomic writes data to path through a temporary file in the same
// directory, fsyncs it, and renames it into place with mode 0600.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+managerStateFileSuffix)
	if err != nil {
		return err
	}
	tmpName := file.Name()
	defer os.Remove(tmpName)

	if err := file.Chmod(managerStateFileMode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	return nil
}

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
