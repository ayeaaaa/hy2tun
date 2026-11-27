package manager

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuditAppendsJSONLineAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	log := NewAuditLog(filepath.Join(dir, "audit.jsonl"))
	require.NoError(t, log.Append(AuditEntry{Action: "mapping.update", ServerName: "us", Target: "m1", Result: "ok"}))
	data, err := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	require.NoError(t, err)
	require.Contains(t, string(data), `"action":"mapping.update"`)
	require.NoError(t, log.Append(AuditEntry{Action: "rotate", ServerName: "us", Result: "error", Code: "invalid_credentials"}))
	require.Error(t, NewAuditLog(filepath.Join(dir, "missing", "audit.jsonl")).Append(AuditEntry{Action: "x"}))
}

func TestAuditFileIsPrivateAndAppendsOneLinePerEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	log := NewAuditLog(path)
	require.NoError(t, log.Append(AuditEntry{Action: "server.add", ServerID: "srv_1", ServerName: "us", Result: "ok"}))
	require.NoError(t, log.Append(AuditEntry{Action: "mapping.update", ServerID: "srv_1", ServerName: "us", Target: "m1", Result: "error", Message: "revision conflict"}))

	require.Equal(t, 0o600, fileMode(t, path))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 2)

	var entry AuditEntry
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &entry))
	require.Equal(t, "mapping.update", entry.Action)
	require.Equal(t, "srv_1", entry.ServerID)
	require.False(t, entry.Time.IsZero())
}

func TestAuditAppendSyncsDirectoryOnCreateAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	originalSyncDir := syncDirFn
	syncDirFn = func(string) error { return errors.New("injected directory sync failure") }
	t.Cleanup(func() { syncDirFn = originalSyncDir })

	err := NewAuditLog(path).Append(AuditEntry{Action: "server.add", Result: "ok"})
	require.ErrorContains(t, err, "sync audit log directory")

	// The line was written before the directory sync failed; a handler would
	// treat the error as fail-closed and not perform the mutation.
	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Contains(t, string(data), `"action":"server.add"`)

	syncDirFn = originalSyncDir
	require.NoError(t, NewAuditLog(path).Append(AuditEntry{Action: "server.edit", Result: "ok"}))
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, strings.Split(strings.TrimSpace(string(data)), "\n"), 2)
}
