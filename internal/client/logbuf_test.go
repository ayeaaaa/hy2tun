package client

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLogBufferKeepsLastN(t *testing.T) {
	buffer := newLogBuffer(logBufferSize)
	for i := 1; i <= 600; i++ {
		_, err := fmt.Fprintf(buffer, "line %d\n", i)
		require.NoError(t, err)
	}

	entries := buffer.Entries("", 0)
	require.Len(t, entries, logBufferSize)
	require.Equal(t, "line 101", entries[0].Message)
	require.Equal(t, "line 600", entries[len(entries)-1].Message)
}

func TestLogBufferIncludesStructuredAttrs(t *testing.T) {
	buffer := newLogBuffer(10)
	logger := slog.New(slog.NewJSONHandler(buffer, nil))

	logger.Warn("reverse UDP receive failed", "mapping", "m1", "error", errors.New("boom"))

	entries := buffer.Entries("", 0)
	require.Len(t, entries, 1)
	require.Equal(t, "WARN", entries[0].Level)
	require.Contains(t, entries[0].Message, "reverse UDP receive failed")
	require.Contains(t, entries[0].Message, "mapping=m1")
	require.Contains(t, entries[0].Message, "error=boom")
}

func TestLogBufferFiltersByLevelAndLimits(t *testing.T) {
	buffer := newLogBuffer(10)
	logger := slog.New(slog.NewJSONHandler(buffer, nil))

	logger.Info("info message")
	logger.Warn("warn message")
	logger.Error("error message")

	require.Len(t, buffer.Entries("", 0), 3)
	require.Len(t, buffer.Entries("info", 0), 3)
	require.Len(t, buffer.Entries("warn", 0), 2)
	require.Len(t, buffer.Entries("error", 0), 1)

	limited := buffer.Entries("", 2)
	require.Len(t, limited, 2)
	require.Equal(t, "warn message", limited[0].Message)
	require.Equal(t, "error message", limited[1].Message)
	require.Equal(t, "WARN", limited[0].Level)
}
