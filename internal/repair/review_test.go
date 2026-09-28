package repair_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/99designs/keyring"

	"github.com/kritama/tama-link/internal/credential/secretservice"
	"github.com/kritama/tama-link/internal/credential/secrettest"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/repair"
)

func TestConcurrentRepairPublishesOneItem(t *testing.T) {
	path, _, items, _ := existingProfile(t, `{"refresh_token":"refresh-token-SUPER-SECRET"}`)
	service := strandedService(t, items)
	started := make(chan struct{})
	release := make(chan struct{})
	service.SetGate(func(ctx context.Context, op string) error {
		if op != "create" {
			return nil
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	})

	first := make(chan error, 1)
	go func() {
		_, err := repair.MigrateLegacyKeyring(context.Background(), service, repairRequest(path))
		first <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first repair did not reach item creation")
	}
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer secondCancel()
	_, err := repair.MigrateLegacyKeyring(secondCtx, service, repairRequest(path))
	if !errors.Is(err, repair.ErrLeaseBusy) {
		t.Fatalf("second repair = %v, want lease contention", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first repair: %v", err)
	}

	stateKey := onlyStateKey(t, items)
	values := service.Collection(defaultCollectionPath).Values(stateKey)
	if len(values) != 1 {
		t.Fatalf("destination copies = %d, want 1", len(values))
	}
	backend, err := secretservice.OpenService(context.Background(), service, secretservice.Config{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := backend.Remove(stateKey); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := service.Collection(defaultCollectionPath).Values(stateKey); len(got) != 0 {
		t.Fatalf("removal left %d items", len(got))
	}
}

func TestRemoveDeletesDuplicateCredentialItems(t *testing.T) {
	t.Parallel()

	service := secrettest.New()
	collection := service.SetDefault(defaultCollectionPath, "Default keyring")
	backend, err := secretservice.OpenService(context.Background(), service, secretservice.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	item := keyring.Item{Key: "demo/default/state/v1-abc", Data: bytes.Repeat([]byte{9}, 32)}
	if err := backend.Set(item); err != nil {
		t.Fatalf("set: %v", err)
	}
	blob, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// replace=false is the Secret Service duplicate path. Plant a second match
	// directly so removal is proven even when a writer did not replace.
	if _, err := collection.CreateItem(context.Background(), sessionStub{}, item.Key, secretservice.Secret{Value: blob, ContentType: "application/json"}, false); err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if len(collection.Values(item.Key)) != 2 {
		t.Fatal("fixture did not retain a duplicate item")
	}
	if err := backend.Remove(item.Key); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(collection.Values(item.Key)) != 0 {
		t.Fatalf("removal left %d items", len(collection.Values(item.Key)))
	}
}

func TestRepairCancellationDoesNotWrite(t *testing.T) {
	path, _, items, _ := existingProfile(t, `{"refresh_token":"refresh-token-SUPER-SECRET"}`)
	service := strandedService(t, items)
	started := make(chan struct{})
	release := make(chan struct{})
	service.SetGate(func(context.Context, string) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := repair.MigrateLegacyKeyring(ctx, service, repairRequest(path))
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("repair did not reach the secret service")
	}
	cancel()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("repair = %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled repair did not return")
	}
	if service.CreateItemCalls() != 0 {
		t.Fatalf("cancelled repair wrote %d items", service.CreateItemCalls())
	}
}

func TestRepairPromptCancelReturnsWithoutWriting(t *testing.T) {
	path, _, items, _ := existingProfile(t, `{"refresh_token":"refresh-token-SUPER-SECRET"}`)
	service := strandedService(t, items)
	service.Collection("/org/freedesktop/secrets/collection/Tama_5fLink").SetLocked(true)
	service.SetGate(func(ctx context.Context, op string) error {
		if op != "unlock" {
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := repair.MigrateLegacyKeyring(ctx, service, repairRequest(path))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repair = %v, want deadline", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("stalled unlock returned after %s", time.Since(started))
	}
	if service.CreateItemCalls() != 0 {
		t.Fatalf("stalled unlock wrote %d items", service.CreateItemCalls())
	}
}

func TestWrongStateKeyDoesNotContaminateDestination(t *testing.T) {
	const secret = "refresh-token-SUPER-SECRET"
	path, identity, items, _ := existingProfile(t, `{"refresh_token":"`+secret+`"}`)
	service := strandedService(t, items)
	stateKey := onlyStateKey(t, items)
	legacy := service.Collection("/org/freedesktop/secrets/collection/Tama_5fLink")
	original, ok := legacy.Get(stateKey)
	if !ok {
		t.Fatal("legacy state key is missing")
	}
	legacy.Put(stateKey, wrongKeyBlob(t, stateKey), "application/json")

	_, err := repair.MigrateLegacyKeyring(context.Background(), service, repairRequest(path))
	if err == nil {
		t.Fatal("wrong state key was accepted")
	}
	if bytes.Contains([]byte(err.Error()), []byte(secret)) {
		t.Fatalf("error %q contains secret material", err)
	}
	if service.CreateItemCalls() != 0 || len(service.Collection(defaultCollectionPath).Values(stateKey)) != 0 {
		t.Fatal("failed validation wrote a destination item")
	}
	legacy.Put(stateKey, original, "application/json")
	result, err := repair.MigrateLegacyKeyring(context.Background(), service, repairRequest(path))
	if err != nil {
		t.Fatalf("retry after restoring the source: %v", err)
	}
	if result.StateKeyID != identity.StateKeyID {
		t.Fatalf("state key id = %s, want %s", result.StateKeyID, identity.StateKeyID)
	}
}

func TestMalformedRequiredRecordDoesNotContaminateDestination(t *testing.T) {
	path, _, items, _ := existingProfile(t, `{"refresh_token":"refresh-token-SUPER-SECRET"}`)
	service := strandedService(t, items)
	secretKey := "demo/default/secret/oauth-refresh-abc"
	legacy := service.Collection("/org/freedesktop/secrets/collection/Tama_5fLink")
	original, ok := legacy.Get(secretKey)
	if !ok {
		t.Fatal("legacy oauth record is missing")
	}
	legacy.Put(secretKey, []byte("not-a-credential-record"), "text/plain")

	_, err := repair.MigrateLegacyKeyring(context.Background(), service, repairRequest(path))
	if err == nil {
		t.Fatal("malformed credential record was accepted")
	}
	if service.CreateItemCalls() != 0 {
		t.Fatal("malformed record was copied")
	}
	legacy.Put(secretKey, original, "application/json")
	if _, err := repair.MigrateLegacyKeyring(context.Background(), service, repairRequest(path)); err != nil {
		t.Fatalf("retry after restoring the source: %v", err)
	}
}

func repairRequest(path string) repair.Request {
	return repair.Request{
		DatabasePath: path,
		Namespace:    "demo/default",
		Limits:       limits.Default(),
		Interactive:  true,
	}
}

func onlyStateKey(t *testing.T, items map[string]keyring.Item) string {
	t.Helper()
	for key := range items {
		if bytes.Contains([]byte(key), []byte("/state/")) {
			return key
		}
	}
	t.Fatal("state key item was not recorded")
	return ""
}

func wrongKeyBlob(t *testing.T, key string) []byte {
	t.Helper()
	blob, err := json.Marshal(keyring.Item{Key: key, Data: bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatalf("encode wrong key: %v", err)
	}
	return blob
}

const defaultCollectionPath = "/org/freedesktop/secrets/collection/Default_5fkeyring"

type sessionStub struct{}

func (sessionStub) Path() string { return "/session/stub" }
