package logging

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoggerWritesJSONAtConfiguredLevel(t *testing.T) {
	var buf bytes.Buffer
	log, closer, err := NewWithWriter("debug", &buf)
	require.NoError(t, err)
	defer closer.Close()
	log.Debug("hello", "key", "value")
	require.Contains(t, buf.String(), `"level":"DEBUG"`)
	require.Contains(t, buf.String(), `"key":"value"`)
}

func TestLoggerFiltersBelowLevel(t *testing.T) {
	var buf bytes.Buffer
	log, closer, err := NewWithWriter("info", &buf)
	require.NoError(t, err)
	defer closer.Close()

	log.Debug("hidden")
	log.Info("shown")

	require.NotContains(t, buf.String(), "hidden")
	require.Contains(t, buf.String(), "shown")
}

func TestNewWritesToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hy2tun.log")
	log, closer, err := New("warn", path)
	require.NoError(t, err)

	log.Warn("file message", "component", "test")
	require.NoError(t, closer.Close())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), `"level":"WARN"`)
	require.Contains(t, string(data), "file message")
	require.Contains(t, string(data), `"component":"test"`)

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
}

func TestRotatingWriterRotatesAndKeepsBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hy2tun.log")
	writer, err := newRotatingWriter(path, 10, 2)
	require.NoError(t, err)
	defer writer.Close()

	_, err = writer.Write([]byte("1234567890"))
	require.NoError(t, err)
	_, err = writer.Write([]byte("abc"))
	require.NoError(t, err)

	current, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "abc", string(current))
	backup, err := os.ReadFile(path + ".1")
	require.NoError(t, err)
	require.Equal(t, "1234567890", string(backup))
}

func TestNewRejectsInvalidLevel(t *testing.T) {
	_, _, err := New("verbose", "")
	require.ErrorContains(t, err, "level")

	var buf bytes.Buffer
	_, _, err = NewWithWriter("verbose", &buf)
	require.ErrorContains(t, err, "level")
}
