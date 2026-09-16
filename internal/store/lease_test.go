package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
)

func TestLeaseClaimIsExclusive(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	owned, err := s.ClaimLease(ctx, "refresh", "owner-a", time.Minute)
	if err != nil {
		t.Fatalf("ClaimLease: %v", err)
	}
	if !owned {
		t.Fatal("first claim did not acquire the lease")
	}

	taken, err := s.ClaimLease(ctx, "refresh", "owner-b", time.Minute)
	if err != nil {
		t.Fatalf("second ClaimLease: %v", err)
	}
	if taken {
		t.Fatal("second owner claimed a live lease")
	}

	renewed, err := s.ClaimLease(ctx, "refresh", "owner-a", time.Minute)
	if err != nil {
		t.Fatalf("renewing ClaimLease: %v", err)
	}
	if !renewed {
		t.Fatal("owner did not re-acquire its own live lease")
	}
}

func TestLeaseClaimDoesNotReportOwnershipAfterItsDeadline(t *testing.T) {
	base := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	calls := 0
	now := func() time.Time {
		calls++
		if calls == 1 {
			return base
		}
		return base.Add(2 * time.Minute)
	}
	path := t.TempDir() + "/state.db"
	s, err := store.Open(context.Background(), path, newMemKeys(), store.Config{Limits: limits.Default(), Now: now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	owned, err := s.ClaimLease(context.Background(), "worker", "owner-a", time.Minute)
	if err != nil {
		t.Fatalf("ClaimLease: %v", err)
	}
	if owned {
		t.Fatal("ClaimLease reported ownership after the written deadline elapsed")
	}
}

func TestExpiredWorkerCannotCaptureTerminalResult(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()
	if _, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	for _, state := range []contract.Status{contract.StatusQueued, contract.StatusRunning} {
		if _, err := s.Transition(ctx, "sub-1", state, store.TransitionDetail{}); err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}
	if owned, err := s.ClaimLease(ctx, "submission/sub-1", "owner-a", time.Minute); err != nil || !owned {
		t.Fatalf("ClaimLease = %v, %v", owned, err)
	}
	clk.Advance(2 * time.Minute)
	result := contract.Result{Content: []contract.ContentBlock{[]byte(`{"type":"text","text":"done"}`)}}
	if _, err := s.CompleteLeased(ctx, "sub-1", "submission/sub-1", "owner-a", result); !errors.Is(err, store.ErrLeaseNotOwned) {
		t.Fatalf("stale CompleteLeased = %v, want ErrLeaseNotOwned", err)
	}
	if owned, err := s.ClaimLease(ctx, "submission/sub-1", "owner-b", time.Minute); err != nil || !owned {
		t.Fatalf("replacement ClaimLease = %v, %v", owned, err)
	}
	if _, err := s.CompleteLeased(ctx, "sub-1", "submission/sub-1", "owner-b", result); err != nil {
		t.Fatalf("replacement CompleteLeased: %v", err)
	}
}

func TestLeaseExpiresAfterTTL(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if owned, _ := s.ClaimLease(ctx, "refresh", "owner-a", time.Minute); !owned {
		t.Fatal("first claim failed")
	}
	clk.Advance(2 * time.Minute)

	taken, err := s.ClaimLease(ctx, "refresh", "owner-b", time.Minute)
	if err != nil {
		t.Fatalf("ClaimLease after expiry: %v", err)
	}
	if !taken {
		t.Fatal("expired lease was not taken over")
	}
}

func TestLeaseClaimDoesNotExpireBeforeMillisecondTTL(t *testing.T) {
	t.Parallel()

	clk := newClock()
	clk.Advance(999*time.Microsecond + 500*time.Nanosecond)
	s, _ := openTestStore(t, newMemKeys(), clk)
	ctx := context.Background()
	if owned, err := s.ClaimLease(ctx, "worker", "owner-a", time.Millisecond); err != nil || !owned {
		t.Fatalf("ClaimLease = %t, %v", owned, err)
	}

	clk.Advance(500 * time.Microsecond)
	if owned, err := s.ClaimLease(ctx, "worker", "owner-b", time.Millisecond); err != nil || owned {
		t.Fatalf("early competing ClaimLease = %t, %v; want false, nil", owned, err)
	}
	clk.Advance(2 * time.Millisecond)
	if owned, err := s.ClaimLease(ctx, "worker", "owner-b", time.Millisecond); err != nil || !owned {
		t.Fatalf("expired competing ClaimLease = %t, %v; want true, nil", owned, err)
	}
}

func TestLeaseRenewDoesNotExpireBeforeMillisecondTTL(t *testing.T) {
	t.Parallel()

	clk := newClock()
	clk.Advance(999*time.Microsecond + 500*time.Nanosecond)
	s, _ := openTestStore(t, newMemKeys(), clk)
	ctx := context.Background()
	if owned, err := s.ClaimLease(ctx, "worker", "owner-a", time.Millisecond); err != nil || !owned {
		t.Fatalf("ClaimLease = %t, %v", owned, err)
	}
	clk.Advance(400 * time.Microsecond)
	if owned, err := s.RenewLease(ctx, "worker", "owner-a", time.Millisecond); err != nil || !owned {
		t.Fatalf("RenewLease = %t, %v", owned, err)
	}

	clk.Advance(700 * time.Microsecond)
	if owned, err := s.ClaimLease(ctx, "worker", "owner-b", time.Millisecond); err != nil || owned {
		t.Fatalf("early competing ClaimLease = %t, %v; want false, nil", owned, err)
	}
	clk.Advance(2 * time.Millisecond)
	if owned, err := s.ClaimLease(ctx, "worker", "owner-b", time.Millisecond); err != nil || !owned {
		t.Fatalf("expired competing ClaimLease = %t, %v; want true, nil", owned, err)
	}
}

func TestExpiredLeaseCannotBeRenewed(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()
	if owned, _ := s.ClaimLease(ctx, "worker", "owner-a", time.Minute); !owned {
		t.Fatal("first claim failed")
	}
	clk.Advance(2 * time.Minute)
	if renewed, err := s.RenewLease(ctx, "worker", "owner-a", time.Minute); err != nil || renewed {
		t.Fatalf("renew expired lease = %v, %v; want false, nil", renewed, err)
	}
}

func TestLeaseValidatesIdentityAndTTL(t *testing.T) {
	t.Parallel()

	s, _ := openTestStore(t, newMemKeys(), newClock())
	for _, input := range []struct {
		name, owner string
		ttl         time.Duration
	}{
		{"", "owner", time.Minute},
		{"worker", "", time.Minute},
		{"worker", "owner", 0},
		{"worker", "owner", time.Nanosecond},
	} {
		if _, err := s.ClaimLease(context.Background(), input.name, input.owner, input.ttl); err == nil {
			t.Fatalf("ClaimLease(%q, %q, %s) succeeded", input.name, input.owner, input.ttl)
		}
	}
}

func TestLeaseRenewAndRelease(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if owned, _ := s.ClaimLease(ctx, "refresh", "owner-a", time.Minute); !owned {
		t.Fatal("first claim failed")
	}

	if renewed, _ := s.RenewLease(ctx, "refresh", "owner-b", time.Minute); renewed {
		t.Fatal("foreign owner renewed the lease")
	}
	if renewed, _ := s.RenewLease(ctx, "refresh", "owner-a", time.Minute); !renewed {
		t.Fatal("owner could not renew its own lease")
	}

	// A foreign release is a no-op: the holder keeps the lease.
	if err := s.ReleaseLease(ctx, "refresh", "owner-b"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if held, _ := s.ClaimLease(ctx, "refresh", "owner-b", time.Minute); held {
		t.Fatal("lease was taken despite a foreign release")
	}

	if err := s.ReleaseLease(ctx, "refresh", "owner-a"); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if held, _ := s.ClaimLease(ctx, "refresh", "owner-b", time.Minute); !held {
		t.Fatal("released lease was not re-claimed")
	}
}

func TestListLeases(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if owned, _ := s.ClaimLease(ctx, "refresh", "owner-a", time.Minute); !owned {
		t.Fatal("first claim failed")
	}
	if owned, _ := s.ClaimLease(ctx, "worker", "owner-a", 2*time.Minute); !owned {
		t.Fatal("second claim failed")
	}

	leases, err := s.ListLeases(ctx, "owner-a")
	if err != nil {
		t.Fatalf("ListLeases: %v", err)
	}
	if len(leases) != 2 {
		t.Fatalf("leases = %v, want two held leases", leases)
	}
	if _, held := leases["refresh"]; !held {
		t.Fatalf("leases = %v, want refresh", leases)
	}
}
