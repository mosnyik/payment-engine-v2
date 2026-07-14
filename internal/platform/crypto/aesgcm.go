// Package crypto is a small at-rest encryption helper for secrets that,
// unlike passwords, must be recoverable in plaintext to be used (e.g. an
// HMAC signing secret the server needs to compute a signature against —
// a one-way hash won't work there the way it does for a password).
//
// This is an interim measure, not a KMS: the encryption key itself comes
// from config (env var), which is exactly the "key colocated with app
// config" pattern ARCHITECTURE.md §2 flags as insufficient for the HD
// wallet's seed key. Using it here for tenant credentials is still a real
// improvement over plaintext storage, but a proper KMS/Vault-backed key is
// the eventual target for anything higher-stakes than this.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

type AESGCM struct {
	gcm cipher.AEAD
}

// NewAESGCM builds an encryptor from a 32-byte key (AES-256).
func NewAESGCM(key []byte) (*AESGCM, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("crypto: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: create gcm: %w", err)
	}
	return &AESGCM{gcm: gcm}, nil
}

// Encrypt returns a hex-encoded nonce||ciphertext.
func (a *AESGCM) Encrypt(plaintext string) (string, error) {
	nonce := make([]byte, a.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("crypto: generate nonce: %w", err)
	}
	ciphertext := a.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return hex.EncodeToString(ciphertext), nil
}

// Decrypt reverses Encrypt.
func (a *AESGCM) Decrypt(encoded string) (string, error) {
	data, err := hex.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("crypto: decode hex: %w", err)
	}
	nonceSize := a.gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("crypto: ciphertext too short")
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := a.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: decrypt: %w", err)
	}
	return string(plaintext), nil
}
