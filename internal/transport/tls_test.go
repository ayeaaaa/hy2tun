package transport

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/testutil"
)

func TestClientTLSPreservesCAAndPin(t *testing.T) {
	// Real TLS handshakes catch disabling CA/hostname checks when a pin is set,
	// skipping the pin when insecure is explicit, or allowing TLS 1.2.
	cert, err := testutil.SelfSignedCert("tunnel.test")
	require.NoError(t, err)
	wrongCert, err := testutil.SelfSignedCert("tunnel.test")
	require.NoError(t, err)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	require.NoError(t, os.WriteFile(caPath, ca, 0600))
	pin := sha256.Sum256(cert.Certificate[0])
	settings := config.ClientTLSConfig{CA: caPath, SNI: "tunnel.test", PinSHA256: hex.EncodeToString(pin[:])}
	cfg, err := ClientTLS(settings, "127.0.0.1:443")
	require.NoError(t, err)
	require.Equal(t, uint16(tls.VersionTLS13), cfg.MinVersion)
	require.Equal(t, "tunnel.test", cfg.ServerName)
	require.NotNil(t, cfg.RootCAs)
	require.False(t, cfg.InsecureSkipVerify)
	require.NoError(t, tlsHandshake(t, cfg, cert, tls.VersionTLS13))
	caErr := tlsHandshake(t, cfg, wrongCert, tls.VersionTLS13)
	require.Error(t, caErr)
	require.False(t, FallbackAllowed(caErr))
	require.Error(t, tlsHandshake(t, cfg, cert, tls.VersionTLS12))

	t.Run("pin still enforced when explicitly insecure", func(t *testing.T) {
		settings.Insecure = true
		cfg, err := ClientTLS(settings, "127.0.0.1:443")
		require.NoError(t, err)
		require.True(t, cfg.InsecureSkipVerify)
		require.NoError(t, tlsHandshake(t, cfg, cert, tls.VersionTLS13))
		require.ErrorIs(t, tlsHandshake(t, cfg, wrongCert, tls.VersionTLS13), ErrCertificatePin)
		require.ErrorIs(t, cfg.VerifyPeerCertificate(nil, nil), ErrCertificatePin)
	})

	t.Run("pin does not bypass CA", func(t *testing.T) {
		wrongPin := sha256.Sum256(wrongCert.Certificate[0])
		cfg, err := ClientTLS(config.ClientTLSConfig{CA: caPath, SNI: "tunnel.test", PinSHA256: hex.EncodeToString(wrongPin[:])}, "127.0.0.1:443")
		require.NoError(t, err)
		require.Error(t, tlsHandshake(t, cfg, wrongCert, tls.VersionTLS13))
		require.ErrorIs(t, tlsHandshake(t, cfg, cert, tls.VersionTLS13), ErrCertificatePin)
	})

	t.Run("pin does not bypass SNI", func(t *testing.T) {
		cfg, err := ClientTLS(config.ClientTLSConfig{CA: caPath, SNI: "wrong.test", PinSHA256: hex.EncodeToString(pin[:])}, "127.0.0.1:443")
		require.NoError(t, err)
		require.Error(t, tlsHandshake(t, cfg, cert, tls.VersionTLS13))
	})
}

func TestClientTLSDefaultsAndInvalidSettings(t *testing.T) {
	for _, tc := range []struct{ server, want string }{
		{" tunnel.test:443 ", "tunnel.test"},
		{"[::1]:443", "::1"},
	} {
		cfg, err := ClientTLS(config.ClientTLSConfig{}, tc.server)
		require.NoError(t, err)
		require.Equal(t, tc.want, cfg.ServerName)
		require.False(t, cfg.InsecureSkipVerify)
	}
	for _, pin := range []string{"bad", strings.Repeat("a", 62), strings.Repeat("z", 64)} {
		_, err := ClientTLS(config.ClientTLSConfig{PinSHA256: pin}, "tunnel.test:443")
		require.Error(t, err)
		require.False(t, FallbackAllowed(err))
	}
	_, err := ClientTLS(config.ClientTLSConfig{CA: filepath.Join(t.TempDir(), "missing")}, "tunnel.test:443")
	require.Error(t, err)
	badCA := filepath.Join(t.TempDir(), "invalid.pem")
	require.NoError(t, os.WriteFile(badCA, []byte("not a certificate"), 0600))
	_, err = ClientTLS(config.ClientTLSConfig{CA: badCA}, "tunnel.test:443")
	require.Error(t, err)

	cert, err := testutil.SelfSignedCert("tunnel.test")
	require.NoError(t, err)
	sum := sha256.Sum256(cert.Certificate[0])
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = hex.EncodeToString([]byte{b})
	}
	cfg, err := ClientTLS(config.ClientTLSConfig{Insecure: true, PinSHA256: " sha256:" + strings.Join(parts, ":") + " "}, "tunnel.test:443")
	require.NoError(t, err)
	require.NoError(t, tlsHandshake(t, cfg, cert, tls.VersionTLS13))
}

// Loopback TLS, not remote network. TCP buffering lets certificate-rejection
// alerts complete even when the server is still writing its handshake flight.
// Deadlines bound failures; both handshake goroutines finish before returning.
func tlsHandshake(t *testing.T, cfg *tls.Config, cert tls.Certificate, maxVersion uint16) error {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	serverDone := make(chan error, 1)
	go func() {
		remote, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer remote.Close()
		serverDone <- tls.Server(remote, &tls.Config{Certificates: []tls.Certificate{cert}, MaxVersion: maxVersion}).HandshakeContext(ctx)
	}()
	local, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	require.NoError(t, err)
	defer local.Close()
	err = tls.Client(local, cfg).HandshakeContext(ctx)
	_ = local.Close()
	<-serverDone
	return err
}
