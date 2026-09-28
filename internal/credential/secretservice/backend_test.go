package secretservice_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/99designs/keyring"

	"github.com/kritama/tama-link/internal/credential/secretservice"
	"github.com/kritama/tama-link/internal/credential/secrettest"
)

const (
	defaultPath = "/org/freedesktop/secrets/collection/Default_5fkeyring"
	defaultName = "Default keyring"
	legacyPath  = "/org/freedesktop/secrets/collection/Tama_5fLink"
)

func TestIndependentOpensReuseDefaultAliasCollection(t *testing.T) {
	t.Parallel()

	service := newAliasedService()
	first, err := secretservice.OpenService(context.Background(), service, secretservice.Config{})
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	second, err := secretservice.OpenService(context.Background(), service, secretservice.Config{})
	if err != nil {
		t.Fatalf("second open: %v", err)
	}

	item := keyring.Item{Key: "demo/default/state/v1-abc", Data: bytes.Repeat([]byte{7}, 32)}
	if err := first.Set(item); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := second.Get(item.Key)
	if err != nil {
		t.Fatalf("get from second process: %v", err)
	}
	if !bytes.Equal(got.Data, item.Data) {
		t.Fatal("second process read a different credential")
	}
	if service.CollectionCount() != 2 {
		t.Fatalf("collections = %d, want the original default and legacy collections", service.CollectionCount())
	}
	if service.UnlockCalls() != 0 {
		t.Fatalf("unlock calls = %d, want 0", service.UnlockCalls())
	}
	legacy := service.Collection(legacyPath)
	if legacy.Len() != 0 {
		t.Fatalf("legacy collection received %d items", legacy.Len())
	}
}

func TestRepeatedOpensDoNotCreateCollections(t *testing.T) {
	t.Parallel()

	service := newAliasedService()
	before := service.CollectionCount()
	for i := 0; i < 5; i++ {
		if _, err := secretservice.OpenService(context.Background(), service, secretservice.Config{}); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	if service.CollectionCount() != before {
		t.Fatalf("collections = %d, want %d", service.CollectionCount(), before)
	}
}

func TestMissingDefaultAliasDoesNotCreateCollection(t *testing.T) {
	t.Parallel()

	service := secrettest.New()
	_, err := secretservice.OpenService(context.Background(), service, secretservice.Config{})
	if !errors.Is(err, secretservice.ErrDefaultCollectionMissing) {
		t.Fatalf("open = %v, want missing default alias", err)
	}
	if service.CollectionCount() != 0 {
		t.Fatalf("collections = %d, want 0", service.CollectionCount())
	}
}

func TestLockedDefaultDoesNotPromptOrCreateWhenUnattended(t *testing.T) {
	t.Parallel()

	service := newAliasedService()
	service.Collection(defaultPath).SetLocked(true)
	backend, err := secretservice.OpenService(context.Background(), service, secretservice.Config{Interactive: false})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	err = backend.Set(keyring.Item{Key: "demo/default/__probe", Data: []byte{1}})
	if !errors.Is(err, secretservice.ErrCollectionLocked) {
		t.Fatalf("set = %v, want locked", err)
	}
	if service.UnlockCalls() != 0 || service.CreateItemCalls() != 0 || service.CollectionCount() != 2 {
		t.Fatalf("unlocks=%d creates=%d collections=%d", service.UnlockCalls(), service.CreateItemCalls(), service.CollectionCount())
	}
}

func TestInteractiveOpenUnlocksExistingDefaultCollection(t *testing.T) {
	t.Parallel()

	service := newAliasedService()
	service.Collection(defaultPath).SetLocked(true)
	backend, err := secretservice.OpenService(context.Background(), service, secretservice.Config{Interactive: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	item := keyring.Item{Key: "demo/default/state/v1-abc", Data: []byte("0123456789abcdef0123456789abcdef")}
	if err := backend.Set(item); err != nil {
		t.Fatalf("set: %v", err)
	}
	if service.UnlockCalls() == 0 {
		t.Fatal("interactive open did not unlock the existing collection")
	}
	if service.CollectionCount() != 2 {
		t.Fatalf("collections = %d, want 2", service.CollectionCount())
	}
	if _, ok := service.Collection(defaultPath).Get(item.Key); !ok {
		t.Fatal("item was not written to the default alias collection")
	}
}

func newAliasedService() *secrettest.Service {
	service := secrettest.New()
	service.SetDefault(defaultPath, defaultName)
	service.AddCollection(legacyPath, secretservice.LegacyCollectionLabel)
	return service
}
