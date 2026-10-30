// Package logging creates the structured JSON logger used by both binaries.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// New returns a JSON logger writing to stdout (file == "") or to file. The
// returned closer closes the file and is always safe to call.
func New(level, file string) (*slog.Logger, io.Closer, error) {
	if _, err := parseLevel(level); err != nil {
		return nil, nil, err
	}
	if file == "" {
		return NewWithWriter(level, os.Stdout)
	}

	writer, err := newRotatingWriter(file, defaultRotateSize, defaultRotateBackups)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file %s: %w", file, err)
	}
	log, _, err := NewWithWriter(level, writer)
	if err != nil {
		_ = writer.Close()
		return nil, nil, err
	}
	return log, writer, nil
}

// NewWithWriter is the test seam for New. It returns a JSON logger at the
// requested level and a no-op closer.
func NewWithWriter(level string, w io.Writer) (*slog.Logger, io.Closer, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, nil, err
	}
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(handler), nopCloser{}, nil
}

func parseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q: must be one of debug, info, warn, error", level)
	}
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
