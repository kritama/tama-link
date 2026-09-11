package store_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
)

// memKeys is an in-memory KeyProvider for tests.
type memKeys struct {
	mu       sync.Mutex
	keys     map[string][]byte
	missing  bool
	creating int
}

func newMemKeys() *memKeys {
	return &memKeys{keys: map[string][]byte{}}
}

func (m *memKeys) GetStateKey(keyID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.missing {
		return nil, store.ErrKeyMissing
	}
	key, ok := m.keys[keyID]
	if !ok {
		return nil, store.ErrKeyMissing
	}
	return key, nil
}

func (m *memKeys) CreateStateKey() (string, []byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id := fmt.Sprintf("state-key-%d", m.creating)
	m.creating++
	m.keys[id] = key
	return id, key, nil
}

func (m *memKeys) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.keys)
}

// downKeys simulates an unavailable credential backend: every key operation
// fails with an error that is not ErrKeyMissing.
type downKeys struct{}

var errBackendDown = errors.New("credential backend down")

func (downKeys) GetStateKey(string) ([]byte, error) { return nil, errBackendDown }
func (downKeys) CreateStateKey() (string, []byte, error) {
	return "", nil, errBackendDown
}

// clock is a deterministic test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock {
	return &clock{t: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func openTestStore(t *testing.T, keys *memKeys, clk *clock) (*store.Store, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "state.db")
	s, err := store.Open(context.Background(), path, keys, store.Config{
		Limits: limits.Default(),
		Now:    clk.Now,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func testSubmission(id, clientRequestID string) store.NewSubmission {
	return store.NewSubmission{
		ID:               id,
		ClientRequestID:  clientRequestID,
		Tool:             "message",
		Strategy:         "upstream_task",
		DescriptorDigest: "sha256:abc",
		Arguments:        []byte(`{"message":"hi"}`),
		ProtocolVersion:  "2025-11-25",
		AdapterVersion:   "tama014/1",
	}
}

func TestOpenCreatesDatabaseAndKey(t *testing.T) {
	t.Parallel()

	keys := newMemKeys()
	clk := newClock()
	s, path := openTestStore(t, keys, clk)
	_ = s

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %o, want 600", info.Mode().Perm())
	}
	if keys.count() != 1 {
		t.Fatalf("keys created = %d, want 1", keys.count())
	}
}

func TestOpenReopensExistingDatabase(t *testing.T) {
	keys := newMemKeys()
	clk := newClock()

	s1, path := openTestStore(t, keys, clk)
	created, err := s1.CreateSubmission(context.Background(), testSubmission("sub-1", "req-1"))
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := store.Open(context.Background(), path, keys, store.Config{Limits: limits.Default(), Now: clk.Now})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	got, err := s2.GetSubmission(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetSubmission after reopen: %v", err)
	}
	if string(got.Arguments) != `{"message":"hi"}` {
		t.Fatalf("arguments = %s, want round-trip through the encrypted blob", got.Arguments)
	}
	if keys.count() != 1 {
		t.Fatalf("keys created = %d, want 1 (existing key must be reused)", keys.count())
	}
}

func TestOpenFailsClosedWithoutKey(t *testing.T) {
	keys := newMemKeys()
	clk := newClock()

	s, path := openTestStore(t, keys, clk)
	if _, err := s.CreateSubmission(context.Background(), testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A fresh credential backend does not know the key: the open must fail
	// closed and leave the database untouched.
	empty := newMemKeys()
	if _, err := store.Open(context.Background(), path, empty, store.Config{Limits: limits.Default()}); !errors.Is(err, store.ErrStateUnavailable) {
		t.Fatalf("Open without key = %v, want ErrStateUnavailable", err)
	}

	reopened, err := store.Open(context.Background(), path, keys, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("reopen with the original key: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if _, err := reopened.GetSubmission(context.Background(), "sub-1"); err != nil {
		t.Fatalf("database damaged by failed open: %v", err)
	}
}

func TestOpenFailsClosedWhenBackendDown(t *testing.T) {
	t.Parallel()

	// A brand-new profile cannot establish its key while the backend is down.
	if _, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"),
		downKeys{}, store.Config{Limits: limits.Default()}); !errors.Is(err, store.ErrStateUnavailable) {
		t.Fatalf("Open new DB with backend down = %v, want ErrStateUnavailable", err)
	}

	// An existing profile with data also fails closed when the backend is down,
	// rather than falling back to plaintext or a replacement key.
	keys := newMemKeys()
	s, path := openTestStore(t, keys, newClock())
	if _, err := s.CreateSubmission(context.Background(), testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := store.Open(context.Background(), path, downKeys{}, store.Config{Limits: limits.Default()}); !errors.Is(err, store.ErrStateUnavailable) {
		t.Fatalf("Open existing DB with backend down = %v, want ErrStateUnavailable", err)
	}
}

func TestOpenRejectsDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := store.Open(context.Background(), dir, newMemKeys(), store.Config{Limits: limits.Default()})
	if err == nil || err.Error() != "state database "+dir+" is a directory" {
		t.Fatalf("Open on directory = %v, want directory error", err)
	}
}

func TestOpenRejectsSymlink(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated privileges on Windows")
	}

	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.db")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(context.Background(), path, newMemKeys(), store.Config{Limits: limits.Default()}); err == nil ||
		err.Error() != "state database "+path+" is not a regular file" {
		t.Fatalf("Open on symlink = %v, want symlink rejection", err)
	}
}

func TestOpenTightensPermissions(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(context.Background(), path, newMemKeys(), store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = s.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %o, want 600 after open", info.Mode().Perm())
	}
}
