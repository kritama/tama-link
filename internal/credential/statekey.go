package credential

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	keyring "github.com/99designs/keyring"

	"github.com/kritama/tama-link/internal/store"
)

// Compile-time guarantee that a Keyring serves as the store's key provider.
var _ store.KeyProvider = (*Keyring)(nil)

// stateKeySize is the AES-256 state-encryption key length.
const stateKeySize = 32

// stateKeyLabel is the human-readable label for a state-key item.
const stateKeyLabel = "state encryption key"

// entryKey namespaces a state key by profile and key identifier.
func (k *Keyring) entryKey(keyID string) string {
	return k.prefix + "state/" + keyID
}

// newKeyID returns a fresh non-secret identifier for a state key.
func newKeyID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("credential state key id: %w", err)
	}
	return "v1-" + hex.EncodeToString(buf), nil
}

// GetStateKey returns the state key previously created under keyID. It returns
// store.ErrKeyMissing when the key is absent and ErrUnavailable when the
// backend is absent or failed. It never generates a replacement key.
func (k *Keyring) GetStateKey(keyID string) ([]byte, error) {
	item, err := k.kr.Get(k.entryKey(keyID))
	if errors.Is(err, keyring.ErrKeyNotFound) {
		return nil, store.ErrKeyMissing
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if len(item.Data) != stateKeySize {
		return nil, fmt.Errorf("%w: state key %q has invalid length %d", ErrUnavailable, keyID, len(item.Data))
	}
	return item.Data, nil
}

// CreateStateKey generates a fresh random state key, stores it in the secure
// backend, and returns its identifier and material. It returns ErrUnavailable
// when the backend is absent or failed.
func (k *Keyring) CreateStateKey() (string, []byte, error) {
	key := make([]byte, stateKeySize)
	if _, err := rand.Read(key); err != nil {
		return "", nil, fmt.Errorf("credential generate state key: %w", err)
	}
	keyID, err := newKeyID()
	if err != nil {
		return "", nil, err
	}
	item := keyring.Item{
		Key:         k.entryKey(keyID),
		Data:        key,
		Label:       stateKeyLabel,
		Description: "Tama Link profile state-encryption key",
	}
	if err := k.kr.Set(item); err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return keyID, key, nil
}
