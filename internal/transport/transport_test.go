package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
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
		{"local send permission denial", &net.OpError{Op: "write", Net: "udp", Err: os.NewSyscallError("sendmsg", syscall.EPERM)}, true},
		{"local read permission denial", &net.OpError{Op: "read", Net: "udp", Err: os.NewSyscallError("recvmsg", syscall.EACCES)}, true},
		{"file permission denial", &os.PathError{Op: "open", Path: "cert.pem", Err: syscall.EACCES}, false},
		{"bare permission denial", syscall.EPERM, false},
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

func TestFallbackAllowedQUICCodes(t *testing.T) {
	// A blanket QUIC deny would miss these network/session-loss cases. Allowing
	// every close instead would promote protocol, crypto and local shutdowns.
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"connection refused", &quic.TransportError{Remote: true, ErrorCode: quic.ConnectionRefused}, true},
		{"no viable path", &quic.TransportError{ErrorCode: quic.NoViablePathError}, true},
		{"clean QUIC peer shutdown", &quic.TransportError{Remote: true, ErrorCode: quic.NoError}, true},
		{"clean HY2 peer shutdown", coreErrs.ClosedError{Err: &quic.ApplicationError{Remote: true, ErrorCode: 0x100}}, true},
		{"local clean HY2 shutdown", &quic.ApplicationError{ErrorCode: 0x100}, false},
		{"local clean QUIC shutdown", &quic.TransportError{ErrorCode: quic.NoError}, false},
		{"QUIC crypto", &quic.TransportError{ErrorCode: 0x12a}, false},
		{"QUIC invalid token", &quic.TransportError{ErrorCode: quic.InvalidToken}, false},
		{"QUIC protocol", &quic.TransportError{ErrorCode: quic.ProtocolViolation}, false},
		{"unknown QUIC transport code", &quic.TransportError{ErrorCode: 0xff}, false},
		{"unknown QUIC application code", &quic.ApplicationError{Remote: true, ErrorCode: 0x102}, false},
		{"protocol stream reset", coreErrs.ClosedError{Err: &quic.StreamError{Remote: true, ErrorCode: 0x101}}, false},
		{"clean stream reset is not session loss", coreErrs.ClosedError{Err: &quic.StreamError{Remote: true, ErrorCode: 0x100}}, false},
		{"unknown cause inside HY2 closed wrapper", coreErrs.ClosedError{Err: errors.New("unclassified operation failure")}, false},
		{"server dial rejection inside HY2 closed wrapper", coreErrs.ClosedError{Err: coreErrs.DialError{Message: "rejected"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, FallbackAllowed(tc.err))
			require.Equal(t, tc.want, FallbackAllowed(fmt.Errorf("wrapped: %w", tc.err)))
			require.Equal(t, tc.want, FallbackAllowed(coreErrs.ClosedError{Err: tc.err}))
		})
	}
}

func TestFallbackAllowedJoinedSecurityDominates(t *testing.T) {
	// Checking only errors.As's first matching QUIC error can miss a denied
	// sibling of the same type. Exercise both sibling orders and nested joins.
	alert := tls.AlertError(42)
	allowed := []struct {
		name string
		err  error
	}{
		{"deadline", context.DeadlineExceeded},
		{"refused", &quic.TransportError{Remote: true, ErrorCode: quic.ConnectionRefused}},
		{"network path lost", &quic.TransportError{ErrorCode: quic.NoViablePathError}},
		{"clean HY2 peer shutdown", &quic.ApplicationError{Remote: true, ErrorCode: 0x100}},
	}
	denied := []struct {
		name string
		err  error
	}{
		{"pointer certificate alert", &alert},
		{"value certificate alert", alert},
		{"protocol stream reset", &quic.StreamError{Remote: true, ErrorCode: 0x101}},
		{"QUIC crypto", &quic.TransportError{ErrorCode: 0x12a}},
		{"QUIC protocol", &quic.TransportError{ErrorCode: quic.ProtocolViolation}},
		{"QUIC application rejection", &quic.ApplicationError{Remote: true, ErrorCode: 0x101}},
		{"unknown QUIC application code", &quic.ApplicationError{Remote: true, ErrorCode: 0x102}},
		{"authentication", ErrAuthentication},
		{"certificate pin", ErrCertificatePin},
	}
	for _, deny := range denied {
		t.Run(deny.name, func(t *testing.T) {
			require.False(t, FallbackAllowed(coreErrs.ClosedError{Err: deny.err}))
			for _, allow := range allowed {
				t.Run(allow.name, func(t *testing.T) {
					require.False(t, FallbackAllowed(errors.Join(allow.err, deny.err)), "allowed cause first")
					require.False(t, FallbackAllowed(errors.Join(deny.err, allow.err)), "denied cause first")
					nested := errors.Join(allow.err, fmt.Errorf("nested: %w", errors.Join(io.EOF, deny.err)))
					require.False(t, FallbackAllowed(coreErrs.ClosedError{Err: nested}), "nested denial")
				})
			}
		})
	}
}
