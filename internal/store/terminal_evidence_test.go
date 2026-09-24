package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
)

func TestCaptureTerminalEvidenceIsAtomicWithTheLease(t *testing.T) {
	t.Parallel()

	st, _ := openTestStore(t, newMemKeys(), newClock())
	created, _, err := st.CreateSubmission(context.Background(), testSubmission("sub-evidence", "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Transition(context.Background(), created.ID, contract.StatusQueued, store.TransitionDetail{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Transition(context.Background(), created.ID, contract.StatusRunning, store.TransitionDetail{}); err != nil {
		t.Fatal(err)
	}
	evidence := []byte(`{"code":-32603,"message":"secret-failure-marker","extension":"ext-marker-9f3a"}`)
	_, err = st.CaptureTerminalLeased(context.Background(), created.ID, "submission/"+created.ID, "not-owner",
		contract.StatusFailed, contract.NewError(contract.CodeUpstreamExecutionFailed, "The upstream task failed."), evidence)
	if !errors.Is(err, store.ErrLeaseNotOwned) {
		t.Fatalf("lost lease = %v, want ErrLeaseNotOwned", err)
	}
	unchanged, err := st.GetSubmission(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != contract.StatusRunning || len(unchanged.TerminalEvidence) != 0 {
		t.Fatalf("lease loss split terminal state: %+v", unchanged.Status)
	}

	owner := "owner-evidence"
	if ok, err := st.ClaimLease(context.Background(), "submission/"+created.ID, owner, time.Minute); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	got, err := st.CaptureTerminalLeased(context.Background(), created.ID, "submission/"+created.ID, owner,
		contract.StatusFailed, contract.NewError(contract.CodeUpstreamExecutionFailed, "The upstream task failed."), evidence)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != contract.StatusFailed || string(got.TerminalEvidence) != string(evidence) {
		t.Fatalf("captured = %s %s", got.Status, got.TerminalEvidence)
	}
}
