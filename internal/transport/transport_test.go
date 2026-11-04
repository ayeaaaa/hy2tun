package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	coreErrs "github.com/apernet/hysteria/core/v2/errors"
	"github.com/apernet/quic-go"
	"github.com/stretchr/testify/require"
)

// These cases catch unsafe fallback on security errors (including errors nested
// inside network failures), and missing fallback for actual network failures.
func TestFallbackAllowed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"auth", coreErrs.AuthError{StatusCode: 401}, false},
		{"auth pointer", &coreErrs.AuthError{StatusCode: 403}, false},
		{"config", coreErrs.ConfigError{Field: "ServerAddr"}, false},
		{"config pointer", &coreErrs.ConfigError{Field: "TLS"}, false},
		{"protocol", coreErrs.ProtocolError{Message: "invalid frame"}, false},
		{"protocol pointer", &coreErrs.ProtocolError{}, false},
		{"invalid certificate", x509.CertificateInvalidError{}, false},
		{"invalid certificate pointer", &x509.CertificateInvalidError{}, false},
		{"unknown CA", x509.UnknownAuthorityError{}, false},
		{"hostname", x509.HostnameError{}, false},
		{"TLS verification", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, false},
		{"TLS record", tls.RecordHeaderError{}, false},
		{"TLS alert in closed HY2", coreErrs.ClosedError{Err: tls.AlertError(42)}, false},
		{"QUIC crypto in closed HY2", coreErrs.ClosedError{Err: &quic.TransportError{ErrorCode: 0x12a}}, false},
		{"QUIC protocol in closed HY2", coreErrs.ClosedError{Err: &quic.TransportError{ErrorCode: quic.ProtocolViolation}}, false},
		{"QUIC application rejection in closed HY2", coreErrs.ClosedError{Err: &quic.ApplicationError{ErrorCode: 0x101}}, false},
		{"QUIC version in closed HY2", coreErrs.ClosedError{Err: &quic.VersionNegotiationError{}}, false},
		{"pin", ErrCertificatePin, false},
		{"authentication sentinel", ErrAuthentication, false},
		{"protocol sentinel", ErrProtocol, false},
		{"queue full", ErrQueueFull, false},
		{"deadline", context.DeadlineExceeded, true},
		{"timeout", &net.OpError{Op: "dial", Net: "udp", Err: timeoutError{}}, true},
		{"QUIC handshake timeout", &quic.HandshakeTimeoutError{}, true},
		{"QUIC idle timeout", &quic.IdleTimeoutError{}, true},
		{"QUIC reset", &quic.StatelessResetError{}, true},
		{"refused", &net.OpError{Op: "dial", Net: "udp", Err: syscall.ECONNREFUSED}, true},
		{"network unreachable", &net.OpError{Op: "dial", Err: syscall.ENETUNREACH}, true},
		{"host unreachable", syscall.EHOSTUNREACH, true},
		{"connection reset", syscall.ECONNRESET, true},
		{"EOF", io.EOF, true},
		{"closed HY2", coreErrs.ClosedError{}, true},
		{"closed HY2 pointer", &coreErrs.ClosedError{}, true},
		{"HY2 connect timeout", coreErrs.ConnectError{Err: context.DeadlineExceeded}, true},
		{"cancelled", context.Canceled, false},
		{"unknown", errors.New("connection refused password=do-not-match-text"), false},
		{"generic network error", &net.OpError{Err: errors.New("other")}, false},
		{"server dial rejection", coreErrs.DialError{Message: "connection refused"}, false},
		{"closed with auth cause", coreErrs.ClosedError{Err: coreErrs.AuthError{}}, false},
		{"joined auth and timeout", errors.Join(ErrAuthentication, context.DeadlineExceeded), false},
		{"joined cancel and EOF", errors.Join(context.Canceled, io.EOF), false},
		{"classified protocol and EOF", Error{Code: "protocol", Cause: io.EOF}, false},
		{"classified auth and timeout", Error{Code: "authentication", Cause: context.DeadlineExceeded}, false},
		{"classified pin and timeout", Error{Code: "certificate_pin", Cause: context.DeadlineExceeded}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, FallbackAllowed(tc.err))
			if tc.err != nil {
				require.Equal(t, tc.want, FallbackAllowed(fmt.Errorf("transport operation: %w", tc.err)))
			}
		})
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "test network timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestErrorPreservesCause(t *testing.T) {
	// Dropping Unwrap would hide both security and network classification.
	err := &Error{Code: "dial", Cause: context.DeadlineExceeded}
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotEmpty(t, err.Error())
	require.NotEmpty(t, (Error{Code: "protocol"}).Error())
	require.True(t, FallbackAllowed(err))
}
