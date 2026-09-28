package repair_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/99designs/keyring"

	"github.com/kritama/tama-link/internal/credential"
	"github.com/kritama/tama-link/internal/credential/secretservice"
	"github.com/kritama/tama-link/internal/credential/secrettest"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/repair"
	"github.com/kritama/tama-link/internal/store"
)

func TestMigrateLegacyKeyringPreservesStateAndOAuth(t *testing.T) {
	t.Parallel()

	const secret = `{"refresh_token":"refresh-token-SUPER-SECRET"}`
	path, identity, items, _ := existingProfile(t, secret)
	service := strandedService(t, items)
	before := service.CollectionCount()

	result, err := repair.MigrateLegacyKeyring(context.Background(), service, repair.Request{
		DatabasePath: path,
		Namespace:    "demo/default",
		Limits:       limits.Default(),
		Interactive:  true,
	})
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if result.StateKeyID != identity.StateKeyID || result.Copied == 0 {
		t.Fatalf("result = %+v, identity %s", result, identity.StateKeyID)
	}
	if service.CollectionCount() != before || service.DeleteCalls() != 0 {
		t.Fatalf("collections=%d deletes=%d", service.CollectionCount(), service.DeleteCalls())
	}

	again, err := repair.MigrateLegacyKeyring(context.Background(), service, repair.Request{
		DatabasePath: path,
		Namespace:    "demo/default",
		Limits:       limits.Default(),
		Interactive:  true,
	})
	if err != nil {
		t.Fatalf("second repair: %v", err)
	}
	if again.Copied != 0 || again.StateKeyID != identity.StateKeyID {
		t.Fatalf("second result = %+v", again)
	}
	after, err := store.ReadPlaintextIdentity(context.Background(), path)
	if err != nil {
		t.Fatalf("identity after repair: %v", err)
	}
	if after.StateKeyID != identity.StateKeyID {
		t.Fatalf("state key id changed from %s to %s", identity.StateKeyID, after.StateKeyID)
	}
}

func TestFailedMigrationDoesNotReplaceStateKey(t *testing.T) {
	t.Parallel()

	const secret = `{"client_secret":"oauth-client-secret-DO-NOT-LOG"}`
	path, identity, _, original := existingProfile(t, secret)
	service := secrettest.New()
	service.SetDefault("/org/freedesktop/secrets/collection/Default_5fkeyring", "Default keyring")
	legacy := service.AddCollection("/org/freedesktop/secrets/collection/Tama_5fLink", secretservice.LegacyCollectionLabel)
	legacy.Put("other-app/password", []byte(secret), "text/plain")

	_, err := repair.MigrateLegacyKeyring(context.Background(), service, repair.Request{
		DatabasePath: path,
		Namespace:    "demo/default",
		Limits:       limits.Default(),
		Interactive:  true,
	})
	if !errors.Is(err, secretservice.ErrLegacyMissing) {
		t.Fatalf("repair = %v, want missing credential", err)
	}
	if bytes.Contains([]byte(err.Error()), []byte(secret)) {
		t.Fatalf("error %q contains secret material", err)
	}
	if service.CreateItemCalls() != 0 || service.DeleteCalls() != 0 {
		t.Fatalf("creates=%d deletes=%d, want no writes", service.CreateItemCalls(), service.DeleteCalls())
	}
	after, err := store.ReadPlaintextIdentity(context.Background(), path)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if after.StateKeyID != identity.StateKeyID {
		t.Fatalf("state key id changed from %s to %s", identity.StateKeyID, after.StateKeyID)
	}
	_, err = store.Open(context.Background(), path, rejectingKeys{}, store.Config{Limits: limits.Default()})
	if !errors.Is(err, store.ErrStateUnavailable) {
		t.Fatalf("open after failed repair = %v, want state unavailable", err)
	}
	if _, err := store.Open(context.Background(), path, original, store.Config{Limits: limits.Default()}); err != nil {
		t.Fatalf("original keyring can no longer open the database: %v", err)
	}
}

type rejectingKeys struct{}

func (rejectingKeys) GetStateKey(string) ([]byte, bool, error) { return nil, false, nil }
func (rejectingKeys) CreateStateKey() (string, []byte, error) {
	return "", nil, errors.New("replacement state key must not be created")
}

func existingProfile(t *testing.T, secret string) (string, store.PlaintextIdentity, map[string]keyring.Item, *credential.Keyring) {
	t.Helper()
	backend := &itemKeyring{items: map[string]keyring.Item{}}
	keys := credential.NewWithBackend("demo/default", backend)
	path := filepath.Join(t.TempDir(), "state.db")
	opened, err := store.Open(context.Background(), path, keys, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, _, err = opened.CreateSubmission(context.Background(), store.NewSubmission{
		ID:               "sub-1",
		ClientRequestID:  "req-1",
		Tool:             "message",
		Strategy:         "upstream_task",
		DescriptorDigest: "sha256:abc",
		Arguments:        []byte(`{"message":"private"}`),
		RequestArguments: []byte(`{"message":"private"}`),
		ProtocolVersion:  "2026-07-28",
		AdapterVersion:   "tama2026/1",
	})
	if err != nil {
		t.Fatalf("create submission: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	insertRepairFence(t, path)
	if err := keys.SetSecret("oauth-refresh-abc", []byte(secret)); err != nil {
		t.Fatalf("store oauth item: %v", err)
	}
	identity, err := store.ReadPlaintextIdentity(context.Background(), path)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return path, identity, backend.items, keys
}

func insertRepairFence(t *testing.T, path string) {
	t.Helper()
	// The fence is plaintext metadata. Inserting it through the store API would
	// also require a lease, which this migration fixture does not need.
	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`INSERT INTO credential_fence (name, generation, slot) VALUES (?, 1, ?)`, store.RefreshFenceName, "oauth-refresh-abc")
	if err != nil {
		t.Fatalf("insert fence: %v", err)
	}
}

func strandedService(t *testing.T, items map[string]keyring.Item) *secrettest.Service {
	t.Helper()
	service := secrettest.New()
	service.SetDefault("/org/freedesktop/secrets/collection/Default_5fkeyring", "Default keyring")
	legacy := service.AddCollection("/org/freedesktop/secrets/collection/Tama_5fLink", secretservice.LegacyCollectionLabel)
	for key, item := range items {
		blob, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("encode item: %v", err)
		}
		legacy.Put(key, blob, "application/json")
	}
	return service
}

type itemKeyring struct {
	items map[string]keyring.Item
}

func (k *itemKeyring) Get(key string) (keyring.Item, error) {
	item, ok := k.items[key]
	if !ok {
		return keyring.Item{}, keyring.ErrKeyNotFound
	}
	return item, nil
}

func (k *itemKeyring) GetMetadata(string) (keyring.Metadata, error) {
	return keyring.Metadata{}, keyring.ErrMetadataNotSupported
}

func (k *itemKeyring) Set(item keyring.Item) error {
	k.items[item.Key] = item
	return nil
}

func (k *itemKeyring) Remove(key string) error {
	delete(k.items, key)
	return nil
}

func (k *itemKeyring) Keys() ([]string, error) {
	keys := make([]string, 0, len(k.items))
	for key := range k.items {
		keys = append(keys, key)
	}
	return keys, nil
}
