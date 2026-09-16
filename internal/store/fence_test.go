package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/store"
)

// fenceCommit advances the fence as leaseOwner in lease epoch
// leaseGeneration, enqueuing previousSlot for retirement retry.
func fenceCommit(ctx context.Context, s *store.Store, generation int64, slot, previousSlot, leaseOwner string, leaseGeneration int64) (bool, error) {
	return s.CommitCredentialFence(ctx, store.CredentialFenceCommit{
		FenceGeneration: generation,
		Slot:            slot,
		PreviousSlot:    previousSlot,
		LeaseName:       "oauth/refresh",
		LeaseOwner:      leaseOwner,
		LeaseGeneration: leaseGeneration,
	})
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
	if ok, err := fenceCommit(ctx, s, 1, "slot-1", "", "owner-a", 1); err != nil || !ok {
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
	if ok, err := fenceCommit(ctx, s, 2, "slot-stale", "", "owner-a", 1); err != nil || ok {
		t.Fatalf("stale writer commit = %v %v, want rejected", ok, err)
	}
	generation, slot, found, err := s.ReadCredentialFence(ctx)
	if err != nil || !found || generation != 1 || slot != "slot-1" {
		t.Fatalf("fence after stale commit = %d %s found:%v err:%v, want slot-1 at 1", generation, slot, found, err)
	}

	// The live owner commits its own generation and takes over the fence.
	if ok, err := fenceCommit(ctx, s, 2, "slot-2", "slot-1", "owner-b", 2); err != nil || !ok {
		t.Fatalf("owner-b commit = %v %v, want committed", ok, err)
	}
	generation, slot, found, err = s.ReadCredentialFence(ctx)
	if err != nil || !found || generation != 2 || slot != "slot-2" {
		t.Fatalf("fence after owner-b commit = %d %s found:%v err:%v", generation, slot, found, err)
	}

	// An expired lease cannot commit either.
	clk.Advance(2 * time.Minute)
	if ok, err := fenceCommit(ctx, s, 3, "slot-3", "", "owner-b", 2); err != nil || ok {
		t.Fatalf("expired-lease commit = %v %v, want rejected", ok, err)
	}
}

// TestCommitCredentialFenceRejectsInsertWithoutLiveLease pins that the
// plain INSERT path — a fence row absent because logout cleared it or no
// authorization ever committed — is gated on the live lease epoch too: a
// stale writer whose SetSecret blocked past its lease cannot reinstall
// credentials behind a logout by creating a fresh fence row.
func TestCommitCredentialFenceRejectsInsertWithoutLiveLease(t *testing.T) {
	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	// The fence row is absent; owner-a's lease expires while its write is
	// blocked.
	if ok, err := s.ClaimLease(ctx, "oauth/refresh", "owner-a", time.Minute); err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	clk.Advance(2 * time.Minute)

	if ok, err := fenceCommit(ctx, s, 1, "slot-stale", "", "owner-a", 1); err != nil || ok {
		t.Fatalf("insert without a live lease = %v %v, want rejected", ok, err)
	}
	if _, _, found, err := s.ReadCredentialFence(ctx); err != nil || found {
		t.Fatalf("fence after stale insert = found:%v err:%v, want absent", found, err)
	}

	// The fresh epoch that takes over can create the fence row.
	if ok, err := s.ClaimLease(ctx, "oauth/refresh", "owner-b", time.Minute); err != nil || !ok {
		t.Fatalf("owner-b claim = %v %v", ok, err)
	}
	if ok, err := fenceCommit(ctx, s, 1, "slot-1", "", "owner-b", 2); err != nil || !ok {
		t.Fatalf("owner-b commit = %v %v, want committed", ok, err)
	}
}

// TestCommitCredentialFenceEnqueuesPreviousSlot pins that the retirement
// record is one transaction with the fence advance: a committed fence
// always carries a durable record for the slot it replaced, so a later
// deletion failure can be retried instead of stranding the old grant.
func TestCommitCredentialFenceEnqueuesPreviousSlot(t *testing.T) {
	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if ok, err := s.ClaimLease(ctx, "oauth/refresh", "owner-a", time.Minute); err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	if ok, err := fenceCommit(ctx, s, 1, "slot-1", "slot-0", "owner-a", 1); err != nil || !ok {
		t.Fatalf("commit = %v %v", ok, err)
	}
	pending, err := s.RetiredCredentialSlots(ctx)
	if err != nil || len(pending) != 1 || pending[0] != "slot-0" {
		t.Fatalf("retirement backlog = %v err=%v, want [slot-0]", pending, err)
	}

	// A rejected commit (stale generation) enqueues nothing: the fence was
	// not advanced.
	if ok, err := fenceCommit(ctx, s, 1, "slot-x", "slot-stale", "owner-a", 1); err != nil || ok {
		t.Fatalf("stale commit = %v %v, want rejected", ok, err)
	}
	pending, err = s.RetiredCredentialSlots(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("retirement backlog after rejected commit = %v err=%v, want unchanged", pending, err)
	}

	// A successful deletion clears the record.
	if err := s.ClearRetiredCredentialSlot(ctx, "slot-0"); err != nil {
		t.Fatalf("ClearRetiredCredentialSlot: %v", err)
	}
	pending, err = s.RetiredCredentialSlots(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("retirement backlog after clear = %v err=%v, want empty", pending, err)
	}
}

// TestClearCredentialFence pins the epoch-bound fence clear: only the
// owner of the claimed lease epoch may clear, an absent fence is
// successfully cleared (a repeated logout or a legacy-only profile has
// nothing to clear), and a lost epoch is refused with the fence intact.
func TestClearCredentialFence(t *testing.T) {
	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	// A held lease with no fence at all clears successfully: a repeated
	// logout or a legacy-only profile has nothing to clear.
	if ok, err := s.ClaimLease(ctx, "oauth/refresh", "owner-a", time.Minute); err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	if cleared, err := s.ClearCredentialFence(ctx, "oauth/refresh", "owner-a", 1, ""); err != nil || !cleared {
		t.Fatalf("absent-fence clear = %v %v, want cleared", cleared, err)
	}
	if ok, err := fenceCommit(ctx, s, 1, "slot-1", "", "owner-a", 1); err != nil || !ok {
		t.Fatalf("commit = %v %v", ok, err)
	}
	if cleared, err := s.ClearCredentialFence(ctx, "oauth/refresh", "owner-a", 1, ""); err != nil || !cleared {
		t.Fatalf("ClearCredentialFence = %v %v, want cleared", cleared, err)
	}
	if _, _, found, err := s.ReadCredentialFence(ctx); err != nil || found {
		t.Fatalf("fence after clear = found:%v err:%v, want none", found, err)
	}

	// A later login commits from a clean fence under its own lease epoch.
	if ok, err := fenceCommit(ctx, s, 1, "slot-2", "", "owner-a", 1); err != nil || !ok {
		t.Fatalf("commit after clear = %v %v, want committed", ok, err)
	}
	if _, slot, found, err := s.ReadCredentialFence(ctx); err != nil || !found || slot != "slot-2" {
		t.Fatalf("fence after relogin = %s found:%v err:%v", slot, found, err)
	}

	// A lost epoch cannot clear a fence installed by the process that
	// took over: owner-b claims, then owner-a's stale clear is refused and
	// the fence survives.
	clk.Advance(2 * time.Minute)
	if ok, err := s.ClaimLease(ctx, "oauth/refresh", "owner-b", time.Minute); err != nil || !ok {
		t.Fatalf("owner-b claim = %v %v", ok, err)
	}
	cleared, err := s.ClearCredentialFence(ctx, "oauth/refresh", "owner-a", 1, "")
	if err != nil || cleared {
		t.Fatalf("stale-epoch clear = %v %v, want refused", cleared, err)
	}
	if _, slot, found, err := s.ReadCredentialFence(ctx); err != nil || !found || slot != "slot-2" {
		t.Fatalf("fence after refused clear = %s found:%v err:%v", slot, found, err)
	}
	pending, err := s.RetiredCredentialSlots(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("retirement backlog after refused clear = %v err=%v, want empty", pending, err)
	}
}

// TestClearCredentialFenceRetiresSlotAtomically pins that the fence clear
// and the retired-slot enqueue are one transaction: a cleared pointer
// always leaves a durable reference to the slot it pointed at, so a crash
// or a later deletion failure can never strand the slot, and a refused
// clear enqueues nothing.
func TestClearCredentialFenceRetiresSlotAtomically(t *testing.T) {
	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if ok, err := s.ClaimLease(ctx, "oauth/refresh", "owner-a", time.Minute); err != nil || !ok {
		t.Fatalf("claim = %v %v", ok, err)
	}
	if ok, err := fenceCommit(ctx, s, 1, "slot-1", "", "owner-a", 1); err != nil || !ok {
		t.Fatalf("commit = %v %v", ok, err)
	}
	if cleared, err := s.ClearCredentialFence(ctx, "oauth/refresh", "owner-a", 1, "slot-1"); err != nil || !cleared {
		t.Fatalf("clear with retired slot = %v %v, want cleared", cleared, err)
	}
	if _, _, found, err := s.ReadCredentialFence(ctx); err != nil || found {
		t.Fatalf("fence after clear = found:%v err:%v, want none", found, err)
	}
	pending, err := s.RetiredCredentialSlots(ctx)
	if err != nil || len(pending) != 1 || pending[0] != "slot-1" {
		t.Fatalf("retirement backlog = %v err=%v, want [slot-1]", pending, err)
	}

	// A refused (stale-epoch) clear with a slot enqueues nothing: the
	// fence it would have pointed at is still live under the new owner.
	clk.Advance(2 * time.Minute)
	if ok, err := s.ClaimLease(ctx, "oauth/refresh", "owner-b", time.Minute); err != nil || !ok {
		t.Fatalf("owner-b claim = %v %v", ok, err)
	}
	if ok, err := fenceCommit(ctx, s, 1, "slot-2", "", "owner-b", 2); err != nil || !ok {
		t.Fatalf("owner-b commit = %v %v", ok, err)
	}
	if cleared, err := s.ClearCredentialFence(ctx, "oauth/refresh", "owner-a", 1, "slot-2"); err != nil || cleared {
		t.Fatalf("stale-epoch clear with retired slot = %v %v, want refused", cleared, err)
	}
	pending, err = s.RetiredCredentialSlots(ctx)
	if err != nil || len(pending) != 1 || pending[0] != "slot-1" {
		t.Fatalf("retirement backlog after refused clear = %v err=%v, want [slot-1] unchanged", pending, err)
	}
}
