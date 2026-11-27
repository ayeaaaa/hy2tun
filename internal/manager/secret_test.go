package manager

import (
	"bytes"
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

	encoded = encoded[:len(encoded)-1] + "x"
	_, err = DecryptSecret(key, encoded)
	require.Error(t, err)
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
