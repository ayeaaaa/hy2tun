package manager

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadConfigDefaultsAndValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.yaml")
	require.NoError(t, os.WriteFile(path, []byte("listen: 127.0.0.1:7600\n"), 0o600))
	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "/", cfg.BasePath)
	require.Equal(t, 30*time.Second, cfg.RefreshInterval)
	require.False(t, cfg.TrustProxy)

	require.NoError(t, os.WriteFile(path, []byte("listen: \"\"\n"), 0o600))
	_, err = LoadConfig(path)
	require.ErrorContains(t, err, "listen")
}

func TestLoadConfigNormalizesBasePathAndRequiresFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.yaml")

	body := "listen: \"127.0.0.1:0\"\n" +
		"base_path: hy2tun\n" +
		"state_file: \" /tmp/state.json \"\n" +
		"key_file: \" /tmp/manager.key \"\n" +
		"audit_file: \" /tmp/audit.jsonl \"\n" +
		"refresh_interval: 1s\n" +
		"trust_proxy: true\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "/hy2tun/", cfg.BasePath)
	require.Equal(t, "/tmp/state.json", cfg.StateFile)
	require.Equal(t, "/tmp/manager.key", cfg.KeyFile)
	require.Equal(t, "/tmp/audit.jsonl", cfg.AuditFile)
	require.Equal(t, time.Second, cfg.RefreshInterval)
	require.True(t, cfg.TrustProxy)

	require.NoError(t, os.WriteFile(path, []byte("state_file: \"\"\n"), 0o600))
	_, err = LoadConfig(path)
	require.ErrorContains(t, err, "state_file")

	require.NoError(t, os.WriteFile(path, []byte("refresh_interval: 500ms\n"), 0o600))
	_, err = LoadConfig(path)
	require.ErrorContains(t, err, "refresh_interval")
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.yaml")
	require.NoError(t, os.WriteFile(path, []byte("listen: 127.0.0.1:7600\nstate_fil: /tmp/typo.json\n"), 0o600))
	_, err := LoadConfig(path)
	require.ErrorContains(t, err, "state_fil")
}

func TestLoadConfigRejectsSharedPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.yaml")

	body := "state_file: /tmp/shared.json\n" +
		"key_file: /tmp/./shared.json\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	_, err := LoadConfig(path)
	require.ErrorContains(t, err, "must not point to the same file")
}
