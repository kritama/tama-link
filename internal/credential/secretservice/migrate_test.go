package secretservice_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kritama/tama-link/internal/credential/secretservice"
	"github.com/kritama/tama-link/internal/credential/secrettest"
)

func TestMigrateCopiesExactStateKeyAndIsIdempotent(t *testing.T) {
	t.Parallel()

	const secret = "refresh-token-SUPER-SECRET"
	service, legacy := legacyService()
	stateKey := "demo/default/state/v1-abcdef"
	blob := []byte("  {\"Data\":\"" + secret + "\",\"unnormalized\":true}")
	legacy.Put(stateKey, blob, "application/json")
	legacy.Put("other-app/password", []byte(secret), "text/plain")

	result, err := secretservice.Migrate(context.Background(), service, migrateState(stateKey))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if result.Copied != 1 || result.AlreadyPresent != 0 {
		t.Fatalf("result = %+v, want one copy", result)
	}
	got, ok := service.Collection(defaultPath).Get(stateKey)
	if !ok || !bytes.Equal(got, blob) {
		t.Fatalf("copied bytes = %q ok=%v, want exact source bytes", got, ok)
	}
	if _, ok := service.Collection(defaultPath).Get("other-app/password"); ok {
		t.Fatal("migration copied an item outside the profile namespace")
	}
	again, err := secretservice.Migrate(context.Background(), service, migrateState(stateKey))
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if again.Copied != 0 || again.AlreadyPresent != 1 {
		t.Fatalf("second result = %+v, want already present", again)
	}
	if source, ok := legacy.Get(stateKey); !ok || !bytes.Equal(source, blob) {
		t.Fatal("migration deleted or rewrote the source item")
	}
	if service.DeleteCalls() != 0 {
		t.Fatalf("delete calls = %d, want 0", service.DeleteCalls())
	}
}

func TestMigrateFailsClosedWithoutWriting(t *testing.T) {
	t.Parallel()

	const secret = "oauth-client-secret-DO-NOT-LOG"
	tests := []struct {
		name    string
		prepare func(*secrettest.Service, *secrettest.Collection)
		req     secretservice.MigrateRequest
		want    error
	}{
		{
			name:    "missing",
			prepare: func(*secrettest.Service, *secrettest.Collection) {},
			req:     migrateState("demo/default/state/v1-missing"),
			want:    secretservice.ErrLegacyMissing,
		},
		{
			name: "conflicting",
			prepare: func(service *secrettest.Service, legacy *secrettest.Collection) {
				legacy.Put("demo/default/state/v1-abc", []byte("one-"+secret), "application/json")
				other := service.AddCollection("/org/freedesktop/secrets/collection/Tama_5fLink_5f1", secretservice.LegacyCollectionLabel)
				other.Put("demo/default/state/v1-abc", []byte("two-"+secret), "application/json")
			},
			req:  migrateState("demo/default/state/v1-abc"),
			want: secretservice.ErrLegacyConflict,
		},
		{
			name: "locked",
			prepare: func(_ *secrettest.Service, legacy *secrettest.Collection) {
				legacy.Put("demo/default/state/v1-abc", []byte(secret), "application/json")
				legacy.SetLocked(true)
			},
			req:  migrateState("demo/default/state/v1-abc"),
			want: secretservice.ErrCollectionLocked,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			service, legacy := legacyService()
			test.prepare(service, legacy)
			before := service.CreateItemCalls()
			_, err := secretservice.Migrate(context.Background(), service, test.req)
			if !errors.Is(err, test.want) {
				t.Fatalf("migrate = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error %q contains secret material", err)
			}
			if service.CreateItemCalls() != before || service.DeleteCalls() != 0 {
				t.Fatalf("creates=%d deletes=%d, want no writes and no deletes", service.CreateItemCalls(), service.DeleteCalls())
			}
			if service.CollectionCount() != 2 && test.name != "conflicting" {
				t.Fatalf("collections = %d", service.CollectionCount())
			}
		})
	}
}

func TestMigrateUnlocksLegacyCollectionOnlyWhenInteractive(t *testing.T) {
	t.Parallel()

	service, legacy := legacyService()
	blob := []byte("exact-state-key-bytes")
	legacy.Put("demo/default/state/v1-abc", blob, "application/json")
	legacy.SetLocked(true)

	result, err := secretservice.Migrate(context.Background(), service, secretservice.MigrateRequest{
		Prefix:      "demo/default/",
		StateKey:    "demo/default/state/v1-abc",
		Interactive: true,
	})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if result.Copied != 1 {
		t.Fatalf("copied = %d, want 1", result.Copied)
	}
	if service.UnlockCalls() == 0 {
		t.Fatal("interactive migration did not unlock the legacy collection")
	}
	got, ok := service.Collection(defaultPath).Get("demo/default/state/v1-abc")
	if !ok || !bytes.Equal(got, blob) {
		t.Fatal("interactive migration did not preserve exact bytes")
	}
	if source, ok := legacy.Get("demo/default/state/v1-abc"); !ok || !bytes.Equal(source, blob) {
		t.Fatal("source item was not retained")
	}
}

func legacyService() (*secrettest.Service, *secrettest.Collection) {
	service := secrettest.New()
	service.SetDefault(defaultPath, defaultName)
	legacy := service.AddCollection(legacyPath, secretservice.LegacyCollectionLabel)
	return service, legacy
}

func migrateState(stateKey string) secretservice.MigrateRequest {
	return secretservice.MigrateRequest{
		Prefix:   "demo/default/",
		StateKey: stateKey,
	}
}
