package credential

import (
	"errors"
	"fmt"
	"regexp"

	keyring "github.com/99designs/keyring"
)

// secretLabelPattern restricts secret labels to opaque, path-free
// identifiers so labels can never traverse the profile namespace.
var secretLabelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// secretEntryKey namespaces a secret by profile and label.
func (k *Keyring) secretEntryKey(label string) string {
	return k.prefix + "secret/" + label
}

// GetSecret returns the secret stored under label for this profile, or
// found=false. It never touches the state-encryption key space.
func (k *Keyring) GetSecret(label string) ([]byte, bool, error) {
	if !secretLabelPattern.MatchString(label) {
		return nil, false, fmt.Errorf("credential: invalid secret label")
	}
	item, err := k.kr.Get(k.secretEntryKey(label))
	if errors.Is(err, keyring.ErrKeyNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return item.Data, true, nil
}

// SetSecret stores data under label for this profile, replacing any
// previous value.
func (k *Keyring) SetSecret(label string, data []byte) error {
	if !secretLabelPattern.MatchString(label) {
		return fmt.Errorf("credential: invalid secret label")
	}
	if len(data) == 0 {
		return fmt.Errorf("credential: secret data must not be empty")
	}
	item := keyring.Item{
		Key:         k.secretEntryKey(label),
		Data:        data,
		Label:       "Tama Link secret " + label,
		Description: "Tama Link profile secret",
	}
	if err := k.kr.Set(item); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// DeleteSecret removes the secret stored under label. An absent label is
// not an error.
func (k *Keyring) DeleteSecret(label string) error {
	if !secretLabelPattern.MatchString(label) {
		return fmt.Errorf("credential: invalid secret label")
	}
	err := k.kr.Remove(k.secretEntryKey(label))
	if errors.Is(err, keyring.ErrKeyNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}
