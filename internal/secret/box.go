// Package secret provides authenticated encryption for recoverable sensitive
// configuration values such as application YAML and environment variables.
//
// The package only handles raw key material: callers read the master key file
// elsewhere and pass the key bytes in. Encryption is AES-256-GCM, so every
// ciphertext carries its own integrity check and can be bound to an
// application and version through the additional authenticated data.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// KeySize is the required length of the master key in bytes.
const KeySize = 32

// ErrInvalidKeySize is returned when the master key has an unexpected length.
var ErrInvalidKeySize = errors.New("secret: master key must be 32 bytes")

// ErrCiphertextTooShort is returned when a ciphertext cannot even hold a
// nonce and an authentication tag.
var ErrCiphertextTooShort = errors.New("secret: ciphertext is too short")

// Box seals and opens sensitive values with a single master key.
type Box struct {
	aead cipher.AEAD
}

// New builds a Box from the given master key. The key must be exactly
// KeySize bytes; anything else is rejected instead of being padded or
// truncated.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("%w, got %d bytes", ErrInvalidKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: create gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plain and returns nonce || ciphertext || tag. A fresh random
// nonce is generated for every call, so sealing the same value twice never
// produces the same output. The aad is authenticated but not encrypted and
// binds the ciphertext to its application and version.
func (b *Box) Seal(plain, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plain, aad), nil
}

// Open decrypts a ciphertext produced by Seal. It fails when the ciphertext
// is truncated, when it was sealed with another key, when it was tampered
// with, or when the aad does not match the one used while sealing.
func (b *Box) Open(ciphertext, aad []byte) ([]byte, error) {
	nonceSize := b.aead.NonceSize()
	if len(ciphertext) < nonceSize+b.aead.Overhead() {
		return nil, fmt.Errorf("%w: %d bytes", ErrCiphertextTooShort, len(ciphertext))
	}
	nonce, payload := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plain, err := b.aead.Open(nil, nonce, payload, aad)
	if err != nil {
		return nil, fmt.Errorf("secret: open: %w", err)
	}
	return plain, nil
}
