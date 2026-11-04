package transport

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"

	"hy2tun/internal/config"
)

// ClientTLS preserves the existing CA/SNI/insecure/certificate-DER pin policy.
// A pin supplements verification, never implicitly disables it. TCP must use
// TLS 1.3 or newer; callers can reuse these verification fields for HY2.
func ClientTLS(cfg config.ClientTLSConfig, server string) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		ServerName:         cfg.SNI,
		InsecureSkipVerify: cfg.Insecure,
	}
	if tlsConfig.ServerName == "" {
		if host, _, err := net.SplitHostPort(strings.TrimSpace(server)); err == nil {
			tlsConfig.ServerName = strings.TrimSpace(host)
		}
	}
	if cfg.CA != "" {
		pemData, err := os.ReadFile(cfg.CA)
		if err != nil {
			return nil, &Error{Code: errConfig.Code, Cause: fmt.Errorf("read TLS CA %s: %w", cfg.CA, err)}
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemData) {
			return nil, &Error{Code: errConfig.Code, Cause: fmt.Errorf("parse TLS CA %s: no certificates found", cfg.CA)}
		}
		tlsConfig.RootCAs = pool
	}
	if cfg.PinSHA256 != "" {
		text := strings.TrimSpace(cfg.PinSHA256)
		text = strings.TrimPrefix(text, "sha256:")
		text = strings.ReplaceAll(text, ":", "")
		pin, err := hex.DecodeString(text)
		if err != nil || len(pin) != sha256.Size {
			return nil, &Error{Code: errConfig.Code, Cause: fmt.Errorf("tls.pin_sha256 must be a 32-byte SHA-256 hex digest")}
		}
		tlsConfig.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return ErrCertificatePin
			}
			sum := sha256.Sum256(rawCerts[0])
			if subtle.ConstantTimeCompare(sum[:], pin) != 1 {
				return ErrCertificatePin
			}
			return nil
		}
	}
	return tlsConfig, nil
}
