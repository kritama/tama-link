package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/store"
)

// fenceCommit commits the fence as leaseOwner in lease epoch leaseGeneration.
func fenceCommit(ctx context.Context, s *store.Store, generation int64, slot, leaseOwner string, leaseGeneration int64) (bool, error) {
	return s.CommitCredentialFence(ctx, generation, slot, "oauth/refresh", leaseOwner, leaseGeneration)
}

// TestCommitCredentialFenceRequiresLiveLease pins the lease-bound fence
// commit: only the current unexpired lease owner, in the ownership epoch it
// claimed, may advance the fence. A writer that lost the lease while its
// secret-store write was blocked can never make its value live — not even
// before the new owner commits its own generation.
func TestCommitCredentialFenceRequiresLiveLease(t *testing.T) {
	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if ok, err := s.ClaimLease(ctx, "oauth/refresh", "owner-a", time.Minute); err != nil || !ok {
		t.Fatalf("owner-a claim = %v %v", ok, err)
	}
	if ok, err := fenceCommit(ctx, s, 1, "slot-1", "owner-a", 1); err != nil || !ok {
		t.Fatalf("owner-a commit = %v %v, want committed", ok, err)
	}

	// owner-a's lease expires; owner-b claims and advances the epoch.
	clk.Advance(2 * time.Minute)
	if ok, err := s.ClaimLease(ctx, "oauth/refresh", "owner-b", time.Minute); err != nil || !ok {
		t.Fatalf("owner-b claim = %v %v", ok, err)
	}

	// The stale writer resumes before owner-b commits anything. Its
	// next-generation commit must be rejected: it no longer owns the
	// lease epoch it captured.
	if ok, err := fenceCommit(ctx, s, 2, "slot-stale", "owner-a", 1); err != nil || ok {
		t.Fatalf("stale writer commit = %v %v, want rejected", ok, err)
	}
	generation, slot, found, err := s.ReadCredentialFence(ctx)
	if err != nil || !found || generation != 1 || slot != "slot-1" {
		t.Fatalf("fence after stale commit = %d %s found:%v err:%v, want slot-1 at 1", generation, slot, found, err)
	}

	// The live owner commits its own generation and takes over the fence.
	if ok, err := fenceCommit(ctx, s, 2, "slot-2", "owner-b", 2); err != nil || !ok {
		t.Fatalf("owner-b commit = %v %v, want committed", ok, err)
	}
	generation, slot, found, err = s.ReadCredentialFence(ctx)
	if err != nil || !found || generation != 2 || slot != "slot-2" {
		t.Fatalf("fence after owner-b commit = %d %s found:%v err:%v", generation, slot, found, err)
	}

	// An expired lease cannot commit either.
	clk.Advance(2 * time.Minute)
	if ok, err := fenceCommit(ctx, s, 3, "slot-3", "owner-b", 2); err != nil || ok {
		t.Fatalf("expired-lease commit = %v %v, want rejected", ok, err)
	}
}

// TestClearCredentialFence pins that logout's fence clearing removes the
// fence row: readers then see no credential at all instead of following a
// dangling pointer to a deleted slot, and a later login starts from a
// clean fence.
func TestClearCredentialFence(t *testing.T) {
	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if ok, err := s.ClaimLease(ctx, "oauth/refresh", "owner-a", time.Minute); err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	if ok, err := fenceCommit(ctx, s, 1, "slot-1", "owner-a", 1); err != nil || !ok {
		t.Fatalf("commit = %v %v", ok, err)
	}
	if err := s.ClearCredentialFence(ctx); err != nil {
		t.Fatalf("ClearCredentialFence: %v", err)
	}
	if _, _, found, err := s.ReadCredentialFence(ctx); err != nil || found {
		t.Fatalf("fence after clear = found:%v err:%v, want none", found, err)
	}

	// A later login commits from a clean fence under its own lease epoch.
	if ok, err := fenceCommit(ctx, s, 1, "slot-2", "owner-a", 1); err != nil || !ok {
		t.Fatalf("commit after clear = %v %v, want committed", ok, err)
	}
	if _, slot, found, err := s.ReadCredentialFence(ctx); err != nil || !found || slot != "slot-2" {
		t.Fatalf("fence after relogin = %s found:%v err:%v", slot, found, err)
	}
}
