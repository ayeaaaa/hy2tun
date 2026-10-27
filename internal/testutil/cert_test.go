package testutil

import (
	"crypto/x509"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSelfSignedCertCoversHostAndLoopback(t *testing.T) {
	cert, err := SelfSignedCert("localhost")
	require.NoError(t, err)
	require.NotNil(t, cert.Leaf)

	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)

	for _, name := range []string{"localhost", "127.0.0.1"} {
		_, err := cert.Leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: pool})
		require.NoError(t, err, "verify %s", name)
	}

	require.WithinDuration(t, time.Now().Add(time.Hour), cert.Leaf.NotAfter, time.Minute)
	require.Equal(t, []string{"localhost"}, cert.Leaf.DNSNames)
}
