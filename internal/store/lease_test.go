package store_test

import (
	"context"
	"testing"
	"time"
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
