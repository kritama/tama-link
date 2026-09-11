package credential

import (
	"errors"
	"sync"
	"testing"

	keyring "github.com/99designs/keyring"

	"github.com/kritama/tama-link/internal/store"
)

// fakeKeyring is an in-memory keyring.Keyring for tests. It can be configured
// to fail Get or Set to exercise the fail-closed paths.
type fakeKeyring struct {
	mu      sync.Mutex
	items   map[string]keyring.Item
	failGet bool
	failSet bool
}

func newFakeKeyring() *fakeKeyring {
	return &fakeKeyring{items: map[string]keyring.Item{}}
}

func (f *fakeKeyring) Get(key string) (keyring.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet {
		return keyring.Item{}, errors.New("backend down")
	}
	it, ok := f.items[key]
	if !ok {
		return keyring.Item{}, keyring.ErrKeyNotFound
	}
	return it, nil
}

func (f *fakeKeyring) GetMetadata(string) (keyring.Metadata, error) {
	return keyring.Metadata{}, keyring.ErrMetadataNotSupported
}

func (f *fakeKeyring) Set(item keyring.Item) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSet {
		return errors.New("backend down")
	}
	f.items[item.Key] = item
	return nil
}

func (f *fakeKeyring) Remove(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, key)
	return nil
}

func (f *fakeKeyring) Keys() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.items))
	for k := range f.items {
		keys = append(keys, k)
	}
	return keys, nil
}

func TestStateKeyRoundTrip(t *testing.T) {
	kr := newWith("alpha", newFakeKeyring())

	keyID, key, err := kr.CreateStateKey()
	if err != nil {
		t.Fatalf("CreateStateKey: %v", err)
	}
	if len(key) != stateKeySize {
		t.Fatalf("key length = %d, want %d", len(key), stateKeySize)
	}
	if keyID == "" {
		t.Fatal("empty key id")
	}

	got, err := kr.GetStateKey(keyID)
	if err != nil {
		t.Fatalf("GetStateKey: %v", err)
	}
	if string(got) != string(key) {
		t.Fatal("round-trip key mismatch")
	}
}

func TestGetStateKeyMissing(t *testing.T) {
	kr := newWith("alpha", newFakeKeyring())

	_, err := kr.GetStateKey("v1-never-created")
	if !errors.Is(err, store.ErrKeyMissing) {
		t.Fatalf("GetStateKey absent = %v, want store.ErrKeyMissing", err)
	}
}

func TestProfileIsolation(t *testing.T) {
	shared := newFakeKeyring()
	alpha := newWith("alpha", shared)
	beta := newWith("beta", shared)

	keyID, _, err := alpha.CreateStateKey()
	if err != nil {
		t.Fatalf("CreateStateKey: %v", err)
	}

	// The same key identifier under a different profile must not resolve to
	// alpha's key.
	if _, err := beta.GetStateKey(keyID); !errors.Is(err, store.ErrKeyMissing) {
		t.Fatalf("cross-profile GetStateKey = %v, want store.ErrKeyMissing", err)
	}
}

func TestGetStateKeyBackendUnavailable(t *testing.T) {
	fake := newFakeKeyring()
	fake.failGet = true
	kr := newWith("alpha", fake)

	_, err := kr.GetStateKey("v1-abc")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("GetStateKey backend-down = %v, want ErrUnavailable", err)
	}
}

func TestCreateStateKeyBackendUnavailable(t *testing.T) {
	fake := newFakeKeyring()
	fake.failSet = true
	kr := newWith("alpha", fake)

	if _, _, err := kr.CreateStateKey(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("CreateStateKey backend-down = %v, want ErrUnavailable", err)
	}
}

func TestGetStateKeyCorruptLength(t *testing.T) {
	fake := newFakeKeyring()
	kr := newWith("alpha", fake)
	if err := fake.Set(keyring.Item{Key: "alpha/state/v1-bad", Data: []byte("short")}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := kr.GetStateKey("v1-bad")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("corrupt-length GetStateKey = %v, want ErrUnavailable", err)
	}
}

func TestNewRequiresProfile(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("New(\"\") succeeded, want error")
	}
}
