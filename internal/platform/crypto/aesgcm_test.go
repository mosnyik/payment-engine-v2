package crypto_test

import (
	"testing"

	"github.com/sirfi/payment-engine-v2/internal/platform/crypto"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	a, err := crypto.NewAESGCM(key)
	if err != nil {
		t.Fatalf("new aesgcm: %v", err)
	}

	plaintext := "super-secret-hmac-key-value"
	ciphertext, err := a.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if ciphertext == plaintext {
		t.Fatal("ciphertext must not equal plaintext")
	}

	got, err := a.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != plaintext {
		t.Fatalf("expected %q, got %q", plaintext, got)
	}
}

func TestEncrypt_DifferentEachTime(t *testing.T) {
	key := make([]byte, 32)
	a, err := crypto.NewAESGCM(key)
	if err != nil {
		t.Fatalf("new aesgcm: %v", err)
	}

	c1, _ := a.Encrypt("same-plaintext")
	c2, _ := a.Encrypt("same-plaintext")
	if c1 == c2 {
		t.Fatal("expected different ciphertexts for the same plaintext (random nonce per call)")
	}
}

func TestNewAESGCM_RejectsWrongKeySize(t *testing.T) {
	if _, err := crypto.NewAESGCM([]byte("too-short")); err == nil {
		t.Fatal("expected an error for a non-32-byte key")
	}
}

func TestDecrypt_WrongKeyFails(t *testing.T) {
	key1 := make([]byte, 32)
	key2 := make([]byte, 32)
	key2[0] = 1

	a1, _ := crypto.NewAESGCM(key1)
	a2, _ := crypto.NewAESGCM(key2)

	ciphertext, err := a1.Encrypt("secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	if _, err := a2.Decrypt(ciphertext); err == nil {
		t.Fatal("expected decrypt with the wrong key to fail")
	}
}
