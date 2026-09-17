package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
)

func TestTransitionLeasedRequiresCurrentOwner(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clk := newClock()
	s, _ := openTestStore(t, newMemKeys(), clk)
	if _, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	if owned, err := s.ClaimLease(ctx, "submission/sub-1", "owner-a", time.Minute); err != nil || !owned {
		t.Fatalf("ClaimLease = %t, %v", owned, err)
	}

	if _, err := s.TransitionLeased(ctx, "sub-1", "submission/sub-1", "owner-b", contract.StatusQueued, store.TransitionDetail{}); !errors.Is(err, store.ErrLeaseNotOwned) {
		t.Fatalf("wrong-owner TransitionLeased = %v, want ErrLeaseNotOwned", err)
	}
	got, err := s.GetSubmission(ctx, "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if got.Status != contract.StatusAccepted {
		t.Fatalf("status after rejected transition = %s, want accepted", got.Status)
	}

	if _, err := s.TransitionLeased(ctx, "sub-1", "submission/sub-1", "owner-a", contract.StatusQueued, store.TransitionDetail{}); err != nil {
		t.Fatalf("live-owner TransitionLeased: %v", err)
	}
	clk.Advance(time.Minute + time.Millisecond)
	if _, err := s.TransitionLeased(ctx, "sub-1", "submission/sub-1", "owner-a", contract.StatusRunning, store.TransitionDetail{}); !errors.Is(err, store.ErrLeaseNotOwned) {
		t.Fatalf("expired-owner TransitionLeased = %v, want ErrLeaseNotOwned", err)
	}
	got, err = s.GetSubmission(ctx, "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission after expiry: %v", err)
	}
	if got.Status != contract.StatusQueued {
		t.Fatalf("status after expired transition = %s, want queued", got.Status)
	}
}
