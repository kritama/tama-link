package store_test

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/kritama/tama-link/internal/store"
)

func TestReadPlaintextIdentityDoesNotCreateDatabase(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing.db")
	_, err := store.ReadPlaintextIdentity(context.Background(), path)
	if !errors.Is(err, store.ErrStateUnavailable) {
		t.Fatalf("read missing database = %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing database stat = %v, want not exist", statErr)
	}
}

func TestReadPlaintextIdentityReportsKeyAndSlots(t *testing.T) {
	t.Parallel()

	keys := newMemKeys()
	opened, path := openTestStore(t, keys, newClock())
	if err := opened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	insertFence(t, path, store.RefreshFenceName, 2, "oauth-refresh-abc")
	insertFence(t, path, store.RefreshFenceName+"-retired:oauth-refresh-old", 0, "oauth-refresh-old")

	identity, err := store.ReadPlaintextIdentity(context.Background(), path)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	if identity.StateKeyID == "" {
		t.Fatal("state key id is empty")
	}
	if len(identity.LiveSlots) != 1 || identity.LiveSlots[0] != "oauth-refresh-abc" {
		t.Fatalf("live slots = %v", identity.LiveSlots)
	}
	if len(identity.RetiredSlots) != 1 || identity.RetiredSlots[0] != "oauth-refresh-old" {
		t.Fatalf("retired slots = %v", identity.RetiredSlots)
	}
}

func insertFence(t *testing.T, path, name string, generation int64, slot string) {
	t.Helper()
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`INSERT INTO credential_fence (name, generation, slot) VALUES (?, ?, ?)`, name, generation, slot)
	if err != nil {
		t.Fatalf("insert fence: %v", err)
	}
}
