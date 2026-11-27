package manager

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEncryptDecryptSecretAndTamper(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	encoded, err := EncryptSecret(key, []byte("upstream-password"))
	require.NoError(t, err)
	plain, err := DecryptSecret(key, encoded)
	require.NoError(t, err)
	require.Equal(t, "upstream-password", string(plain))

	// Flip a valid hex nibble in the ciphertext so the failure comes from GCM
	// authentication, not from hex decoding.
	tampered := flipLastHexByte(t, encoded)
	_, err = DecryptSecret(key, tampered)
	require.ErrorContains(t, err, "decrypt secret")
}

func TestEncryptSecretUsesFreshNonceAndRejectsBadKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, secretKeySize)
	first, err := EncryptSecret(key, []byte("same-plaintext"))
	require.NoError(t, err)
	second, err := EncryptSecret(key, []byte("same-plaintext"))
	require.NoError(t, err)
	require.NotEqual(t, first, second, "each encryption must use a fresh nonce")

	_, err = EncryptSecret(key[:secretKeySize-1], []byte("value"))
	require.ErrorContains(t, err, "32 bytes")

	_, err = DecryptSecret(key[:secretKeySize-1], first)
	require.ErrorContains(t, err, "32 bytes")

	wrongKey := bytes.Repeat([]byte{0x24}, secretKeySize)
	_, err = DecryptSecret(wrongKey, first)
	require.Error(t, err)
}

// flipLastHexByte decodes a hex string, flips one bit in its final byte, and
// re-encodes it. The result is still valid hex, so AES-GCM authentication must
// reject it.
func flipLastHexByte(t *testing.T, encoded string) string {
	t.Helper()
	raw, err := hex.DecodeString(encoded)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	raw[len(raw)-1] ^= 0x01
	return hex.EncodeToString(raw)
}
