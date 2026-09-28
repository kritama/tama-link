package repair_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/kritama/tama-link/internal/credential/secretservice"
	"github.com/kritama/tama-link/internal/repair"
)

func TestRepairIgnoresUnrequestedLegacyItems(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"malformed", "parseable", "conflicting"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			path, identity, items, _ := existingProfile(t, `{"refresh_token":"fixture-token"}`)
			service := strandedService(t, items)
			legacy := service.Collection("/org/freedesktop/secrets/collection/Tama_5fLink")
			key := "demo/default/__probe_stale"
			value := []byte("not JSON")
			if kind == "parseable" {
				value = []byte(`{"Data":"cHJvYmU="}`)
			}
			legacy.Put(key, value, "application/json")
			if kind == "conflicting" {
				other := service.AddCollection("/org/freedesktop/secrets/collection/Tama_5fLink_5f1", secretservice.LegacyCollectionLabel)
				other.Put(key, []byte("different unrelated value"), "application/json")
			}
			result, err := repair.MigrateLegacyKeyring(context.Background(), service, repairRequest(path))
			if err != nil {
				t.Fatalf("repair with %s unrequested item: %v", kind, err)
			}
			if result.StateKeyID != identity.StateKeyID || result.Copied != len(items) {
				t.Fatalf("result=%+v, want original state key and %d requested copies", result, len(items))
			}
			if _, ok := service.Collection(defaultCollectionPath).Get(key); ok {
				t.Fatal("repair published an unrequested item")
			}
			if source, ok := legacy.Get(key); !ok || !bytes.Equal(source, value) {
				t.Fatal("repair changed an unrequested source item")
			}
		})
	}
}
