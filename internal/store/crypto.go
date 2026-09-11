package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// KeyProvider supplies the profile-scoped state encryption key from the
// platform credential backend.
type KeyProvider interface {
	// GetStateKey returns the key previously created under keyID and whether it
	// exists. Missing keys are distinct from backend failures.
	GetStateKey(keyID string) (key []byte, found bool, err error)
	// CreateStateKey stores a new random key and returns its identifier and
	// the key material.
	CreateStateKey() (keyID string, key []byte, err error)
}

// encryptionFormat is the on-disk blob format version.
const encryptionFormat = 2

// maxKeyID bounds the non-secret key identifier stored in metadata.
const maxKeyID = 128

// stateCipher seals and opens sensitive submission blobs with AES-256-GCM.
// Each blob is a random 96-bit nonce followed by the ciphertext.
type stateCipher struct {
	aead cipher.AEAD
}

func newStateCipher(key []byte) (stateCipher, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return stateCipher{}, fmt.Errorf("state key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return stateCipher{}, fmt.Errorf("state key: %w", err)
	}
	return stateCipher{aead: aead}, nil
}

func (c stateCipher) seal(plaintext []byte, submissionID, kind string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("state nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, blobAAD(submissionID, kind)), nil
}

func (c stateCipher) open(blob []byte, submissionID, kind string) ([]byte, error) {
	return c.openWithAAD(blob, blobAAD(submissionID, kind))
}

func (c stateCipher) openLegacy(blob []byte) ([]byte, error) {
	return c.openWithAAD(blob, []byte("tama-link/blob/v1"))
}

func (c stateCipher) openWithAAD(blob, aad []byte) ([]byte, error) {
	if len(blob) <= c.aead.NonceSize() {
		return nil, errors.New("state blob is too short")
	}
	nonce, data := blob[:c.aead.NonceSize()], blob[c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, data, aad)
	if err != nil {
		return nil, fmt.Errorf("state blob: %w", err)
	}
	return plaintext, nil
}

// blobAAD binds ciphertext to its format, submission, and semantic column so
// copied database blobs cannot be substituted for one another.
func blobAAD(submissionID, kind string) []byte {
	return []byte(fmt.Sprintf("tama-link/blob/v%d/%s/%s", encryptionFormat, submissionID, kind))
}
