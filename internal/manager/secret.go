package manager

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
)

// secretKeySize is the AES-256 key size used for upstream credentials.
const secretKeySize = 32

// EncryptSecret encrypts plaintext with AES-256-GCM and returns a hex-encoded
// nonce||ciphertext value. A fresh random nonce is generated for every call.
func EncryptSecret(key, plaintext []byte) (string, error) {
	aead, err := newSecretGCM(key)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate secret nonce: %w", err)
	}
	sealed := aead.Seal(nil, nonce, plaintext, nil)

	payload := make([]byte, 0, len(nonce)+len(sealed))
	payload = append(payload, nonce...)
	payload = append(payload, sealed...)
	return hex.EncodeToString(payload), nil
}

// DecryptSecret decrypts a value produced by EncryptSecret. Any malformed,
// truncated, or tampered value (including a mismatched key) returns an error.
func DecryptSecret(key []byte, encoded string) ([]byte, error) {
	aead, err := newSecretGCM(key)
	if err != nil {
		return nil, err
	}

	payload, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode secret: %w", err)
	}
	if len(payload) < aead.NonceSize() {
		return nil, fmt.Errorf("decode secret: ciphertext is too short")
	}

	nonce := payload[:aead.NonceSize()]
	ciphertext := payload[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret: %w", err)
	}
	return plaintext, nil
}

func newSecretGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != secretKeySize {
		return nil, fmt.Errorf("secret key must be %d bytes, got %d", secretKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create AES-GCM: %w", err)
	}
	return aead, nil
}
