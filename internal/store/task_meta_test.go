package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
)

func TestSaveTaskRecordPreservesWideMillisecondIntegers(t *testing.T) {
	t.Parallel()

	st, _ := openTestStore(t, newMemKeys(), newClock())
	created, _, err := st.CreateSubmission(context.Background(), testSubmission("sub-wide", "wide"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Transition(context.Background(), created.ID, contract.StatusQueued, store.TransitionDetail{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Transition(context.Background(), created.ID, contract.StatusRunning, store.TransitionDetail{}); err != nil {
		t.Fatal(err)
	}
	const maxSafe = int64(9_007_199_254_740_991)
	owner := "owner-wide"
	if ok, err := st.ClaimLease(context.Background(), "submission/"+created.ID, owner, time.Minute); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	err = st.SaveTaskRecordLeased(context.Background(), created.ID, "submission/"+created.ID, owner, store.TaskRecord{
		TTLMs:          maxSafe,
		PollIntervalMs: maxSafe,
		Capabilities:   `{"client":{"extensions":{"io.modelcontextprotocol/tasks":{}}}}`,
		UpdatedAt:      "2026-09-11T10:00:01Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSubmission(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TaskTTLMs != maxSafe || got.TaskPollIntervalMs != maxSafe {
		t.Fatalf("ttl %d poll %d", got.TaskTTLMs, got.TaskPollIntervalMs)
	}
}
