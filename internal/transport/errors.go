package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"syscall"

	coreErrs "github.com/apernet/hysteria/core/v2/errors"
	"github.com/apernet/quic-go"
)

// Error carries a stable machine-readable classification while retaining the
// original cause. Classification never depends on potentially sensitive text.
type Error struct {
	Code  string
	Cause error
}

func (e Error) Error() string {
	if e.Cause == nil {
		return e.Code
	}
	return e.Code + ": " + e.Cause.Error()
}

func (e Error) Unwrap() error { return e.Cause }

// Is lets classified errors retain their sentinel identity when a cause is
// attached, including when the cause itself looks like a network failure.
func (e Error) Is(target error) bool {
	switch target := target.(type) {
	case *Error:
		return target != nil && e.Code != "" && e.Code == target.Code
	case Error:
		return e.Code != "" && e.Code == target.Code
	default:
		return false
	}
}

var (
	ErrAuthentication = &Error{Code: "authentication"}
	ErrProtocol       = &Error{Code: "protocol"}
	ErrCertificatePin = &Error{Code: "certificate_pin"}
	ErrQueueFull      = &Error{Code: "queue_full"}
	errConfig         = &Error{Code: "config"}
	errTLS            = &Error{Code: "tls"}
)

// FallbackAllowed permits only recognized network failures or a lost session.
// Security, authentication, protocol, configuration and cancellation errors
// take precedence, even when joined with or wrapping a timeout/EOF.
func FallbackAllowed(err error) bool {
	if err == nil {
		return false
	}
	for _, denied := range []error{context.Canceled, ErrAuthentication, ErrProtocol, ErrCertificatePin, ErrQueueFull, errConfig, errTLS} {
		if errors.Is(err, denied) {
			return false
		}
	}
	if hasError[coreErrs.AuthError](err) || hasError[*coreErrs.AuthError](err) ||
		hasError[coreErrs.ConfigError](err) || hasError[*coreErrs.ConfigError](err) ||
		hasError[coreErrs.ProtocolError](err) || hasError[*coreErrs.ProtocolError](err) ||
		hasError[x509.CertificateInvalidError](err) || hasError[*x509.CertificateInvalidError](err) ||
		hasError[x509.UnknownAuthorityError](err) || hasError[*x509.UnknownAuthorityError](err) ||
		hasError[x509.HostnameError](err) || hasError[*x509.HostnameError](err) ||
		hasError[*tls.CertificateVerificationError](err) ||
		hasError[tls.RecordHeaderError](err) || hasError[*tls.RecordHeaderError](err) || hasError[tls.AlertError](err) ||
		hasError[*quic.TransportError](err) || hasError[*quic.ApplicationError](err) || hasError[*quic.VersionNegotiationError](err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) ||
		hasError[coreErrs.ClosedError](err) || hasError[*coreErrs.ClosedError](err) || hasError[*quic.StatelessResetError](err) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return true
	}
	for _, networkFailure := range []error{syscall.ECONNREFUSED, syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ECONNRESET, syscall.EPIPE} {
		if errors.Is(err, networkFailure) {
			return true
		}
	}
	return false
}

func hasError[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}
