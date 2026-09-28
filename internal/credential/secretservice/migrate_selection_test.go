package secretservice_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/kritama/tama-link/internal/credential/secretservice"
)

func TestMigrateCopiesOnlyRequestedKeys(t *testing.T) {
	t.Parallel()
	service, legacy := legacyService()
	req := secretservice.MigrateRequest{
		Prefix:   "demo/default/",
		StateKey: "demo/default/state/v1-current",
		Required: []string{"demo/default/secret/live-client"},
		Optional: []string{"demo/default/secret/retired", "demo/default/secret/missing"},
	}
	wanted := map[string][]byte{
		req.StateKey:    []byte("current state key"),
		req.Required[0]: []byte("live credential"),
		req.Optional[0]: []byte("retired credential"),
	}
	for key, value := range wanted {
		legacy.Put(key, value, "application/json")
	}
	unrelated := map[string][]byte{
		"demo/default/state/v1-old":       []byte("old state key"),
		"demo/default/__probe_stale":      []byte("unrelated non-JSON value"),
		"demo/default/secret/unrequested": []byte(`{"Data":"b3RoZXI="}`),
	}
	for key, value := range unrelated {
		legacy.Put(key, value, "application/json")
	}

	result, err := secretservice.Migrate(context.Background(), service, req)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	destination := service.Collection(defaultPath)
	if result.Copied != len(wanted) || destination.Len() != len(wanted) {
		t.Fatalf("copied=%d destination=%d, want %d requested items", result.Copied, destination.Len(), len(wanted))
	}
	for key, value := range wanted {
		if got, ok := destination.Get(key); !ok || !bytes.Equal(got, value) {
			t.Fatalf("requested item %q did not preserve its exact bytes", key)
		}
	}
	for key, value := range unrelated {
		if _, ok := destination.Get(key); ok {
			t.Fatalf("copied unrequested item %q", key)
		}
		if got, ok := legacy.Get(key); !ok || !bytes.Equal(got, value) {
			t.Fatalf("changed unrequested source item %q", key)
		}
	}
	if _, ok := destination.Get(req.Optional[1]); ok {
		t.Fatal("created an absent optional item")
	}
}
