package encryption

import (
	"crypto/rand"
	"fmt"
	"io"
)

// GenerateDEK generates a random 32-byte data encryption key.
func GenerateDEK() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("failed to generate DEK: %w", err)
	}
	return key, nil
}

// WrapKey encrypts a plaintext key (DEK) with a key encryption key (KEK).
// Returns the wrapped key (ciphertext with prepended nonce).
func WrapKey(dek, kek []byte) ([]byte, error) {
	return Encrypt(dek, kek)
}

// UnwrapKey decrypts a wrapped key with a key encryption key (KEK).
func UnwrapKey(wrapped, kek []byte) ([]byte, error) {
	return Decrypt(wrapped, kek)
}
