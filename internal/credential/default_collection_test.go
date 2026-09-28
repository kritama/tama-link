package credential

import (
	"bytes"
	"context"
	"testing"

	"github.com/99designs/keyring"

	"github.com/kritama/tama-link/internal/credential/secretservice"
	"github.com/kritama/tama-link/internal/credential/secrettest"
)

func TestLoginThenServeReadsSharedSecretServiceRecords(t *testing.T) {
	service := secrettest.New()
	service.SetDefault("/org/freedesktop/secrets/collection/Default_5fkeyring", "Default keyring")
	service.AddCollection("/org/freedesktop/secrets/collection/Tama_5fLink", secretservice.LegacyCollectionLabel)
	before := service.CollectionCount()

	previous := openPlatformBackend
	openPlatformBackend = func(interactive bool) (keyring.Keyring, error) {
		return secretservice.OpenService(context.Background(), service, secretservice.Config{Interactive: interactive})
	}
	t.Cleanup(func() { openPlatformBackend = previous })

	login, err := NewInteractive("tama-app/default")
	if err != nil {
		t.Fatalf("login backend: %v", err)
	}
	keyID, key, err := login.CreateStateKey()
	if err != nil {
		t.Fatalf("create state key: %v", err)
	}
	refresh := []byte("refresh-token-SUPER-SECRET")
	client := []byte("oauth-client-secret-DO-NOT-LOG")
	if err := login.SetSecret("oauth-refresh", refresh); err != nil {
		t.Fatalf("set refresh: %v", err)
	}
	if err := login.SetSecret("oauth-client", client); err != nil {
		t.Fatalf("set client: %v", err)
	}

	serve, err := New("tama-app/default")
	if err != nil {
		t.Fatalf("serve backend: %v", err)
	}
	got, found, err := serve.GetStateKey(keyID)
	if err != nil || !found || !bytes.Equal(got, key) {
		t.Fatalf("serve state key found=%v err=%v equal=%v", found, err, bytes.Equal(got, key))
	}
	gotRefresh, found, err := serve.GetSecret("oauth-refresh")
	if err != nil || !found || !bytes.Equal(gotRefresh, refresh) {
		t.Fatalf("serve refresh found=%v err=%v", found, err)
	}
	gotClient, found, err := serve.GetSecret("oauth-client")
	if err != nil || !found || !bytes.Equal(gotClient, client) {
		t.Fatalf("serve client found=%v err=%v", found, err)
	}
	if service.CollectionCount() != before {
		t.Fatalf("collections = %d, want %d", service.CollectionCount(), before)
	}
	if service.Collection("/org/freedesktop/secrets/collection/Tama_5fLink").Len() != 0 {
		t.Fatal("login wrote credentials into a legacy Tama Link collection")
	}
}
