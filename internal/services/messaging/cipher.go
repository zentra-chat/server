package messaging

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"io"

	"github.com/zentra/server/pkg/encryption"
)

type ContentCipher interface {
	Encrypt(content string, key []byte) (ciphertext []byte, nonce []byte, err error)
	Decrypt(ciphertext []byte, nonce []byte, key []byte) (string, error)
}

type ChannelCipher struct{}

type DMCipher struct{}

func NewChannelCipher() *ChannelCipher {
	return &ChannelCipher{}
}

func NewDMCipher() *DMCipher {
	return &DMCipher{}
}

func (c *ChannelCipher) Encrypt(content string, key []byte) ([]byte, []byte, error) {
	ciphertext, err := encryption.Encrypt([]byte(content), key)
	if err != nil {
		return nil, nil, err
	}
	return ciphertext, nil, nil
}

func (c *ChannelCipher) Decrypt(ciphertext []byte, _ []byte, key []byte) (string, error) {
	plaintext, err := encryption.Decrypt(ciphertext, key)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func (c *DMCipher) Encrypt(content string, key []byte) ([]byte, []byte, error) {
	if len(key) != 32 {
		return nil, nil, encryption.ErrInvalidKeyLength
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}

	ciphertext := gcm.Seal(nil, nonce, []byte(content), nil)
	return ciphertext, nonce, nil
}

func (c *DMCipher) Decrypt(ciphertext, nonce, key []byte) (string, error) {
	if len(key) != 32 {
		return "", encryption.ErrInvalidKeyLength
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", encryption.ErrDecryptionFailed
	}

	return string(plaintext), nil
}
