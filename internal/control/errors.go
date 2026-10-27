package control

import "errors"

// ErrorCode is a stable error code carried in protocol messages. The values
// are part of the wire contract; see spec §6.4.
type ErrorCode string

// Stable protocol error codes from spec §6.4.
const (
	CodeUnauthorized        ErrorCode = "unauthorized"
	CodeUnsupportedProtocol ErrorCode = "unsupported_protocol"
	CodeBadMessage          ErrorCode = "bad_message"
	CodePortNotAllowed      ErrorCode = "port_not_allowed"
	CodePortReserved        ErrorCode = "port_reserved"
	CodePortInUse           ErrorCode = "port_in_use"
	CodeNoFreePort          ErrorCode = "no_free_port"
	CodeInvalidTarget       ErrorCode = "invalid_target"
	CodeConflict            ErrorCode = "conflict"
	CodeStaleRevision       ErrorCode = "stale_revision"
	CodeInternal            ErrorCode = "internal"
)

var (
	// ErrFrameTooLarge reports a frame whose declared or encoded payload
	// exceeds MaxFrameSize.
	ErrFrameTooLarge = errors.New("control: frame too large")
	// ErrBadMessage reports an empty, malformed, or otherwise invalid frame
	// payload. A truncated body is reported as io.ErrUnexpectedEOF instead.
	ErrBadMessage = errors.New("control: bad message")
	// ErrUnknownType reports a frame whose "type" field is not a known
	// control message type.
	ErrUnknownType = errors.New("control: unknown message type")

	// ErrConflict reports a configuration revision conflict during a push.
	ErrConflict = errors.New("control: revision conflict")
	// ErrClientOffline reports that the target client has no live control
	// stream.
	ErrClientOffline = errors.New("control: client offline")
	// ErrPushTimeout reports that the client did not acknowledge a push in
	// time.
	ErrPushTimeout = errors.New("control: push timed out")
)
