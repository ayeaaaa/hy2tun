package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	// WebSecretFileName is the per-configuration web session signing key.
	WebSecretFileName = "web_secret"

	webSecretSize       = 32
	webPasswordLength   = 20
	webPasswordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

// EnsureWebPassword generates a 20-character password and stores its bcrypt
// hash in web.PasswordHash when no hash is configured. The plaintext is
// returned exactly once so the caller can print it; it is never persisted.
func EnsureWebPassword(web *WebConfig) (string, bool, error) {
	if web == nil {
		return "", false, fmt.Errorf("web config is nil")
	}
	if web.Username == "" {
		web.Username = "admin"
	}
	if web.PasswordHash != "" {
		if _, err := bcrypt.Cost([]byte(web.PasswordHash)); err != nil {
			return "", false, fmt.Errorf("web.password_hash is not a valid bcrypt hash: %w", err)
		}
		return "", false, nil
	}

	password, err := randomPassword(webPasswordLength)
	if err != nil {
		return "", false, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", false, fmt.Errorf("hash web password: %w", err)
	}
	web.PasswordHash = string(hash)
	return password, true, nil
}

func randomPassword(length int) (string, error) {
	if length <= 0 {
		return "", fmt.Errorf("password length must be positive")
	}
	limit := big.NewInt(int64(len(webPasswordAlphabet)))
	var builder strings.Builder
	builder.Grow(length)
	for i := 0; i < length; i++ {
		index, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("generate web password: %w", err)
		}
		builder.WriteByte(webPasswordAlphabet[index.Int64()])
	}
	return builder.String(), nil
}

// WebSecretPath returns the session key file next to configPath.
func WebSecretPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), WebSecretFileName)
}

// LoadOrCreateWebSecret reads a 32-byte web session key from path, creating it
// with mode 0600 on first use. It is safe against two processes racing to
// create the file: the loser re-reads the winner's key.
func LoadOrCreateWebSecret(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("web secret path is empty")
	}

	key, err := readWebSecret(path)
	if err == nil {
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}

	key = make([]byte, webSecretSize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate web secret: %w", err)
	}
	encoded := []byte(hex.EncodeToString(key) + "\n")

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if os.IsExist(err) {
			// Another process won the creation race. It may not have finished
			// writing yet, so retry briefly before reporting a corrupt key.
			for attempt := 0; attempt < 20; attempt++ {
				key, readErr := readWebSecret(path)
				if readErr == nil {
					return key, nil
				}
				time.Sleep(5 * time.Millisecond)
			}
			return readWebSecret(path)
		}
		return nil, fmt.Errorf("create web secret %s: %w", path, err)
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("write web secret %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("sync web secret %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("close web secret %s: %w", path, err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("sync web secret directory: %w", err)
	}
	return key, nil
}

func readWebSecret(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm() != 0600 {
		if err := os.Chmod(path, 0600); err != nil {
			return nil, fmt.Errorf("secure web secret %s: %w", path, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("parse web secret %s: %w", path, err)
	}
	if len(key) != webSecretSize {
		return nil, fmt.Errorf("parse web secret %s: expected %d bytes, got %d", path, webSecretSize, len(key))
	}
	return key, nil
}
