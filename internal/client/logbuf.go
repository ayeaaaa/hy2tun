package client

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"hy2tun/internal/view"
)

// logBufferSize is the number of recent log entries exposed by the client web
// console.
const logBufferSize = 500

// logBuffer is a fixed-size ring of recent log entries. It implements io.Writer
// so a slog JSON handler can write directly into it.
type logBuffer struct {
	mu      sync.Mutex
	entries []view.LogEntry
	start   int
	count   int
}

func newLogBuffer(size int) *logBuffer {
	if size <= 0 {
		size = logBufferSize
	}
	return &logBuffer{entries: make([]view.LogEntry, size)}
}

// Write parses one or more newline-delimited log records. A JSON record (as
// produced by slog's JSON handler) preserves its timestamp, level, message, and
// structured attributes; any other line is stored as an informational message.
func (b *logBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.appendLocked(parseLogLine(line))
	}
	return len(p), nil
}

// standardLogFields are consumed into dedicated LogEntry fields. Every other
// top-level JSON key is a structured attribute and is appended to the message.
var standardLogFields = map[string]struct{}{
	"time":   {},
	"level":  {},
	"msg":    {},
	"source": {},
}

func parseLogLine(line string) view.LogEntry {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &fields); err == nil && fields != nil && len(fields) > 0 {
		entry := view.LogEntry{
			Time:    time.Now(),
			Level:   strings.ToUpper(jsonString(fields["level"])),
			Message: jsonString(fields["msg"]),
		}
		if rawTime := jsonString(fields["time"]); rawTime != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, rawTime); err == nil {
				entry.Time = parsed
			}
		}
		if attrs := formatLogAttrs(fields); attrs != "" {
			if entry.Message != "" {
				entry.Message += " "
			}
			entry.Message += attrs
		}
		return entry
	}
	return view.LogEntry{Time: time.Now(), Level: "INFO", Message: line}
}

func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

// formatLogAttrs renders non-standard fields as key=value pairs in a stable
// order so the log tab shows mapping IDs, errors, and other slog attributes.
func formatLogAttrs(fields map[string]json.RawMessage) string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		if _, standard := standardLogFields[key]; standard {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+jsonValue(fields[key]))
	}
	return strings.Join(parts, " ")
}

func jsonValue(raw json.RawMessage) string {
	if value := jsonString(raw); value != "" {
		return value
	}
	return strings.TrimSpace(string(raw))
}

func (b *logBuffer) appendLocked(entry view.LogEntry) {
	if len(b.entries) == 0 {
		return
	}
	if b.count < len(b.entries) {
		index := (b.start + b.count) % len(b.entries)
		b.entries[index] = entry
		b.count++
		return
	}
	b.entries[b.start] = entry
	b.start = (b.start + 1) % len(b.entries)
}

// Entries returns recent entries at or above level, oldest first. An empty or
// "all" level returns every entry. A positive limit keeps the newest entries.
func (b *logBuffer) Entries(level string, limit int) []view.LogEntry {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	wanted := logLevelRank(level)
	out := make([]view.LogEntry, 0, b.count)
	for i := 0; i < b.count; i++ {
		entry := b.entries[(b.start+i)%len(b.entries)]
		if wanted > 0 && logLevelRank(entry.Level) < wanted {
			continue
		}
		out = append(out, entry)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func logLevelRank(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return 1
	case "info":
		return 2
	case "warn", "warning":
		return 3
	case "error":
		return 4
	default:
		return 0
	}
}

// teeHandler sends every accepted record to two handlers. Enabled is decided by
// the primary handler so the ring contains exactly the records the configured
// logger would emit.
type teeHandler struct {
	primary   slog.Handler
	secondary slog.Handler
}

func (h teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.primary.Enabled(ctx, level)
}

func (h teeHandler) Handle(ctx context.Context, record slog.Record) error {
	primaryErr := h.primary.Handle(ctx, record)
	secondaryErr := h.secondary.Handle(ctx, record)
	if primaryErr != nil {
		return primaryErr
	}
	return secondaryErr
}

func (h teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return teeHandler{primary: h.primary.WithAttrs(attrs), secondary: h.secondary.WithAttrs(attrs)}
}

func (h teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{primary: h.primary.WithGroup(name), secondary: h.secondary.WithGroup(name)}
}
