package credential

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	keyring "github.com/99designs/keyring"

	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
)

var _ store.KeyProvider = (*Keyring)(nil)

// fakeKeyring is an in-memory keyring.Keyring for tests. It can be configured
// to fail Get or Set to exercise the fail-closed paths.
type fakeKeyring struct {
	mu             sync.Mutex
	items          map[string]keyring.Item
	failGet        bool
	failSet        bool
	pendingRemoves int
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
	f.pendingRemoves++
	f.mu.Unlock()
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

	got, found, err := kr.GetStateKey(keyID)
	if err != nil {
		t.Fatalf("GetStateKey: %v", err)
	}
	if !found {
		t.Fatal("created key reported missing")
	}
	if string(got) != string(key) {
		t.Fatal("round-trip key mismatch")
	}
}

func TestGetStateKeyMissing(t *testing.T) {
	kr := newWith("alpha", newFakeKeyring())

	_, found, err := kr.GetStateKey("v1-never-created")
	if err != nil || found {
		t.Fatalf("GetStateKey absent = found %v, error %v; want false, nil", found, err)
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
	if _, found, err := beta.GetStateKey(keyID); err != nil || found {
		t.Fatalf("cross-profile GetStateKey = found %v, error %v; want false, nil", found, err)
	}
}

func TestGetStateKeyBackendUnavailable(t *testing.T) {
	fake := newFakeKeyring()
	fake.failGet = true
	kr := newWith("alpha", fake)

	_, _, err := kr.GetStateKey("v1-abc")
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

	_, _, err := kr.GetStateKey("v1-bad")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("corrupt-length GetStateKey = %v, want ErrUnavailable", err)
	}
}

func TestNewRequiresProfile(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("New(\"\") succeeded, want error")
	}
}

func TestKeyringSecuresStore(t *testing.T) {
	backend := newFakeKeyring()
	keys := newWith("profile/state", backend)
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(context.Background(), path, keys, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := store.Open(context.Background(), path, keys, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = reopened.Close()
}

// blockingKeyring never completes a Set, modeling a backend that accepts a
// connection but waits on user interaction for writes.
type blockingKeyring struct{ started chan struct{} }

func (b *blockingKeyring) Get(string) (keyring.Item, error)             { panic("unused") }
func (b *blockingKeyring) GetMetadata(string) (keyring.Metadata, error) { panic("unused") }
func (b *blockingKeyring) Set(keyring.Item) error {
	close(b.started)
	select {}
}
func (b *blockingKeyring) Remove(string) error     { panic("unused") }
func (b *blockingKeyring) Reset() error            { panic("unused") }
func (b *blockingKeyring) Keys() ([]string, error) { panic("unused") }

func TestProbeTimesOutOnBlockingBackend(t *testing.T) {
	blocking := &blockingKeyring{started: make(chan struct{})}
	probeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { probeTimeout = 5 * time.Second })

	start := time.Now()
	err := probeBackend("demo", blocking)
	if err == nil {
		t.Fatal("probe succeeded, want timeout error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("probe took %s, want it to be bounded", time.Since(start))
	}
	select {
	case <-blocking.started:
	default:
		t.Fatal("probe never attempted a Set")
	}
}

func TestProbeSucceedsAndCleansUp(t *testing.T) {
	backend := newFakeKeyring()
	if err := probeBackend("demo", backend); err != nil {
		t.Fatalf("probe: %v", err)
	}
	backend.mu.Lock()
	count := len(backend.items)
	backend.mu.Unlock()
	if count != 0 {
		t.Fatalf("probe left %d entries behind", count)
	}
}

// TestProbeKeysAreUniquePerInvocation proves concurrent same-profile
// starts cannot interfere with each other's availability probes: every
// probe uses its own unguessable key, and a probe that sees another
// probe's Remove between its Set and Get still succeeds on its own key.
func TestProbeKeysAreUniquePerInvocation(t *testing.T) {
	seen := make(chan string, 8)
	backend := newFakeKeyring()
	racy := &spyKeyring{inner: backend, seen: seen}

	errs := make(chan error, 4)
	for range 4 {
		go func() { errs <- probeBackend("demo", racy) }()
	}
	// All four probes completed, so exactly their 12 Set/Get/Remove sends
	// are done. Every probe used a distinct per-invocation key.
	setKeys := map[string]bool{}
	for range 12 {
		select {
		case k := <-seen:
			if !strings.HasPrefix(k, "demo/__probe_") {
				t.Fatalf("probe key %q is not per-invocation", k)
			}
			setKeys[k] = true
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for recorded probe keys")
		}
	}
	if len(setKeys) != 4 {
		t.Fatalf("probes used %d distinct keys, want 4 (one per invocation)", len(setKeys))
	}
}

// spyKeyring records every key its inner keyring handles so a test can
// assert probe keys are per-invocation.
type spyKeyring struct {
	inner keyring.Keyring
	seen  chan string
}

func (s *spyKeyring) Set(item keyring.Item) error {
	s.seen <- item.Key
	return s.inner.Set(item)
}

func (s *spyKeyring) Get(key string) (keyring.Item, error) {
	s.seen <- key
	return s.inner.Get(key)
}

func (s *spyKeyring) GetMetadata(key string) (keyring.Metadata, error) {
	return s.inner.GetMetadata(key)
}

func (s *spyKeyring) Remove(key string) error { s.seen <- key; return s.inner.Remove(key) }
func (s *spyKeyring) Keys() ([]string, error) { return s.inner.Keys() }
