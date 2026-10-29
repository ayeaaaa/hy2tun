// Package state persists the server's last-known view of registered clients
// and their mappings so that offline clients remain visible after a restart.
//
// The on-disk document contains only client metadata: names, versions,
// revisions, mapping definitions, and reservation timestamps. Secrets such as
// control tokens and passwords are never stored here.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"hy2tun/internal/config"
)

// ClientState is the last-known state of one registered client.
type ClientState struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Version       string           `json:"version"`
	Revision      int64            `json:"revision"`
	Online        bool             `json:"online"`
	LastSeen      time.Time        `json:"last_seen"`
	Mappings      []config.Mapping `json:"mappings"`
	ReservedUntil time.Time        `json:"reserved_until"`
}

// State is the persisted root document.
type State struct {
	Clients map[string]*ClientState `json:"clients"`
}

// Store owns the in-memory state and its on-disk representation. All methods
// are safe for concurrent use.
type Store struct {
	mu   sync.RWMutex
	path string
	// offlineReserve is read on every online-to-offline transition so a hot
	// configuration update applies without restarting the process.
	offlineReserve atomic.Int64
	state          State
	dirty          bool
}

// Open loads the state file at path. A missing file yields an empty state; a
// file that cannot be read or parsed returns an error. An empty path keeps the
// store in memory and disables persistence.
//
// offlineReserve is how long a port stays reserved for a client after it goes
// offline.
func Open(path string, offlineReserve time.Duration) (*Store, error) {
	s := &Store{
		path:  path,
		state: State{Clients: make(map[string]*ClientState)},
	}
	s.offlineReserve.Store(int64(offlineReserve))
	if path == "" {
		return s, nil
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &s.state); err != nil {
			return nil, fmt.Errorf("state: parse %s: %w", path, err)
		}
	case os.IsNotExist(err):
		return s, nil
	default:
		return nil, fmt.Errorf("state: read %s: %w", path, err)
	}

	if s.state.Clients == nil {
		s.state.Clients = make(map[string]*ClientState)
	}
	return s, nil
}

// Snapshot returns a deep copy of the current state. Callers may read and
// modify the result without affecting the store.
func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

func (s *Store) snapshotLocked() State {
	out := State{Clients: make(map[string]*ClientState, len(s.state.Clients))}
	for id, client := range s.state.Clients {
		if client == nil {
			continue
		}
		out.Clients[id] = client.clone()
	}
	return out
}

// Upsert stores a copy of client, replacing any previous state with the same
// ID. The caller retains ownership of the argument.
func (s *Store) Upsert(client ClientState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.Clients == nil {
		s.state.Clients = make(map[string]*ClientState)
	}
	s.state.Clients[client.ID] = client.clone()
	s.dirty = true
}

// DeleteAndSave removes a client and atomically persists the resulting state.
// If persistence fails, the in-memory client is restored so a retry remains
// possible and the caller does not observe a partially applied deletion.
func (s *Store) DeleteAndSave(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state.Clients == nil {
		return false, nil
	}
	client, ok := s.state.Clients[id]
	if !ok {
		return false, nil
	}
	delete(s.state.Clients, id)
	if err := s.saveLocked(); err != nil {
		s.state.Clients[id] = client
		s.dirty = true
		return false, err
	}
	return true, nil
}

// SetOfflineReserve replaces how long ports stay reserved for clients that go
// offline after this call. Existing reservations are not recalculated.
func (s *Store) SetOfflineReserve(reserve time.Duration) {
	if s == nil {
		return
	}
	s.offlineReserve.Store(int64(reserve))
}

// SetOnline records that a client is online or offline. LastSeen is always set
// to now. When the client goes offline, its ports stay reserved for the
// store's current offlineReserve duration. Unknown clients are ignored.
func (s *Store) SetOnline(id string, online bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	client := s.state.Clients[id]
	if client == nil {
		return
	}
	client.Online = online
	client.LastSeen = now
	if !online {
		client.ReservedUntil = now.Add(time.Duration(s.offlineReserve.Load()))
	}
	s.dirty = true
}

// Reserved reports whether port is soft-reserved for clientID at now. A
// reservation is active only while the client is offline, the port appears in
// the client's mappings, and the reservation has not expired.
func (s *Store) Reserved(clientID string, port int, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	client := s.state.Clients[clientID]
	if client == nil || client.Online {
		return false
	}
	if !now.Before(client.ReservedUntil) {
		return false
	}
	for _, mapping := range client.Mappings {
		if mapping.RemotePort == port {
			return true
		}
	}
	return false
}

// Save atomically writes the current state to disk with mode 0600, keeping the
// previous file as path+".bak". An in-memory store (empty path) is a no-op.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		s.dirty = false
		return nil
	}

	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal %s: %w", s.path, err)
	}
	data = append(data, '\n')

	// Preserve the previous document before replacing it. A read failure other
	// than a missing file aborts the save so no state is silently lost.
	previous, readErr := os.ReadFile(s.path)
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("state: read previous %s: %w", s.path, readErr)
	}

	tmp := s.path + ".tmp"
	// The temporary file is only useful until the rename succeeds, and any
	// partial write must not be left behind.
	defer os.Remove(tmp)
	if err := writeFileSync(tmp, data); err != nil {
		return fmt.Errorf("state: write %s: %w", tmp, err)
	}

	if readErr == nil {
		if err := writeFileSync(s.path+".bak", previous); err != nil {
			return fmt.Errorf("state: write backup %s: %w", s.path+".bak", err)
		}
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("state: rename %s: %w", s.path, err)
	}
	if err := syncDir(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("state: sync %s: %w", filepath.Dir(s.path), err)
	}
	s.dirty = false
	return nil
}

// Close flushes pending changes to disk. It is safe to call more than once.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.dirty {
		return nil
	}
	return s.saveLocked()
}

// clone returns a copy that shares no mutable memory with the receiver.
func (c *ClientState) clone() *ClientState {
	out := *c
	if c.Mappings != nil {
		out.Mappings = append([]config.Mapping(nil), c.Mappings...)
	}
	return &out
}

// writeFileSync writes data to path with mode 0600 and fsyncs the file before
// returning.
func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// syncDir flushes a directory entry so a rename survives a crash. Windows does
// not support syncing directory handles; the rename is still atomic there.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		if runtime.GOOS == "windows" {
			return nil
		}
		return err
	}
	return nil
}
