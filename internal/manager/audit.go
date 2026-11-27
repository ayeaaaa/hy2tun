package manager

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditEntry is one manager action record. Secrets must never be placed in
// Message or any other field.
type AuditEntry struct {
	Time       time.Time `json:"time"`
	Actor      string    `json:"actor,omitempty"`
	ServerID   string    `json:"server_id,omitempty"`
	ServerName string    `json:"server_name,omitempty"`
	Action     string    `json:"action"`
	Target     string    `json:"target,omitempty"`
	Result     string    `json:"result"`
	Code       string    `json:"code,omitempty"`
	Message    string    `json:"message,omitempty"`
}

// AuditLog appends JSON objects to an append-only file. Every append is
// fsynced so a mutation can treat an audit error as fatal (fail closed).
type AuditLog struct {
	path string
	mu   sync.Mutex
	// dirSynced records that the audit directory entry has been fsynced. It
	// starts false per process so a failed first append can retry the directory
	// sync on the next write instead of requiring file re-creation.
	dirSynced bool
}

// NewAuditLog returns an audit log that writes to path. The file is created
// lazily on the first append with mode 0600.
func NewAuditLog(path string) *AuditLog {
	return &AuditLog{path: path}
}

// Append writes one JSON line and syncs it to disk before returning.
func (l *AuditLog) Append(entry AuditEntry) error {
	if l == nil {
		return fmt.Errorf("audit log is nil")
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode audit entry: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	file, created, err := openAuditFile(l.path)
	if err != nil {
		return fmt.Errorf("open audit log %s: %w", l.path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure audit log %s: %w", l.path, err)
	}
	before, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("inspect audit log %s: %w", l.path, err)
	}
	previousSize := before.Size()
	if _, err := file.Write(line); err != nil {
		// A short or failed append can leave a partial JSON line. Roll the
		// file back to the last complete entry before returning the
		// fail-closed error.
		_ = file.Truncate(previousSize)
		_ = file.Sync()
		_ = file.Close()
		return fmt.Errorf("write audit log %s: %w", l.path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync audit log %s: %w", l.path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close audit log %s: %w", l.path, err)
	}
	if created || !l.dirSynced {
		if err := syncDirFn(filepath.Dir(l.path)); err != nil {
			return fmt.Errorf("sync audit log directory %s: %w", filepath.Dir(l.path), err)
		}
		l.dirSynced = true
	}
	return nil
}

// openAuditFile creates the audit file if needed and reports whether this call
// created it. O_EXCL makes the created flag race-safe.
func openAuditFile(path string) (*os.File, bool, error) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		return file, true, nil
	}
	if !os.IsExist(err) {
		return nil, false, err
	}
	file, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, false, err
	}
	return file, false, nil
}
