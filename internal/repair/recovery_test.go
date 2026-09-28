package repair_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/kritama/tama-link/internal/repair"
)

func TestInterruptedRepairResumesExactCopies(t *testing.T) {
	t.Parallel()
	path, identity, items, _ := existingProfile(t, `{"refresh_token":"fixture-token"}`)
	service := strandedService(t, items)
	legacy := service.Collection("/org/freedesktop/secrets/collection/Tama_5fLink")
	destination := service.Collection(defaultCollectionPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.SetGate(func(ctx context.Context, op string) error {
		if op == "create" && service.CreateItemCalls() == 1 {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	_, err := repair.MigrateLegacyKeyring(ctx, service, repairRequest(path))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted repair = %v, want cancellation", err)
	}
	if destination.Len() != 1 || legacy.Len() != len(items) {
		t.Fatalf("destination=%d source=%d, want one partial copy and intact source", destination.Len(), legacy.Len())
	}
	service.SetGate(nil)
	result, err := repair.MigrateLegacyKeyring(context.Background(), service, repairRequest(path))
	if err != nil {
		t.Fatalf("retry repair: %v", err)
	}
	if result.StateKeyID != identity.StateKeyID || result.AlreadyPresent != 1 || result.Copied != len(items)-1 {
		t.Fatalf("retry = %+v, want original state key and only missing items copied", result)
	}
	for key := range items {
		source, ok := legacy.Get(key)
		values := destination.Values(key)
		if !ok || len(values) != 1 || !bytes.Equal(values[0], source) {
			t.Fatalf("retry did not preserve one exact destination copy of %q", key)
		}
	}
	if service.DeleteCalls() != 0 || service.CreateItemCalls() != len(items) {
		t.Fatalf("deletes=%d creates=%d, want no deletes and one write per item", service.DeleteCalls(), service.CreateItemCalls())
	}
}
