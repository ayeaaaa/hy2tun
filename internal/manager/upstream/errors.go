package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Stable upstream JSON error codes the manager branches on. Do not classify a
// 409 by status alone: a mapping push conflict has a different operator action
// than an offline client or an exposure bind conflict.
const (
	CodeConflict         = "conflict"
	CodeClientOffline    = "client_offline"
	CodeClientNotReady   = "client_not_ready"
	CodePushRejected     = "push_rejected"
	CodePartialApply     = "partial_apply"
	CodeExposureConflict = "exposure_conflict"
)

// APIError is a non-2xx upstream response. Code is the server's JSON code
// verbatim; it is empty when the server did not supply one. Message never
// contains a request password or token.
type APIError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

// Error implements error. It never contains credentials, only the upstream
// status, code, sanitized message, and rate-limit delay.
func (e *APIError) Error() string {
	if e == nil {
		return "upstream API error"
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "upstream API error (status %d", e.Status)
	if e.Code != "" {
		fmt.Fprintf(&builder, ", code %s", e.Code)
	}
	builder.WriteString(")")
	if e.Message != "" {
		builder.WriteString(": ")
		builder.WriteString(e.Message)
	}
	if e.RetryAfter > 0 {
		fmt.Fprintf(&builder, " (retry after %s)", e.RetryAfter)
	}
	return builder.String()
}

const maxErrorMessageRunes = 512

// newAPIError decodes the server's JSON error shape. The documented hy2tund
// shape is {error, code, results?, ...}; some proxies and older endpoints use
// {message}. Unknown bodies fall back to the HTTP status text rather than
// echoing arbitrary HTML.
func newAPIError(status int, header http.Header, body []byte, secrets ...string) *APIError {
	var payload struct {
		Error      string `json:"error"`
		Message    string `json:"message"`
		Code       string `json:"code"`
		RetryAfter int64  `json:"retry_after"`
	}
	_ = json.Unmarshal(body, &payload)

	message := strings.TrimSpace(payload.Error)
	if message == "" {
		message = strings.TrimSpace(payload.Message)
	}
	if message == "" {
		message = http.StatusText(status)
	}
	message = truncateRunes(redactSecrets(message, secrets), maxErrorMessageRunes)

	retryAfter := retryAfterFromHeader(header)
	if retryAfter == 0 && payload.RetryAfter > 0 {
		retryAfter = time.Duration(payload.RetryAfter) * time.Second
	}
	return &APIError{
		Status:     status,
		Code:       strings.TrimSpace(payload.Code),
		Message:    message,
		RetryAfter: retryAfter,
	}
}

func retryAfterFromHeader(header http.Header) time.Duration {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
}

// redactSecrets removes both raw and form-encoded occurrences of a credential
// from operator-facing text. The adapter never generates messages with these
// values; this is defense in depth for a reverse proxy or future server version
// that echoes a request body in an error.
func redactSecrets(message string, secrets []string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[REDACTED]")
		if encoded := url.QueryEscape(secret); encoded != secret {
			message = strings.ReplaceAll(message, encoded, "[REDACTED]")
		}
	}
	return message
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}
