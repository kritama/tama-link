package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/submission"
)

func TestSubmissionPreservesAcceptedLifecycleLimitsAcrossReopen(t *testing.T) {
	keys, clk := newMemKeys(), newClock()
	accepted := limits.Default()
	path := t.TempDir() + "/state.db"
	ctx := context.Background()

	s, err := store.Open(ctx, path, keys, store.Config{Limits: accepted, Now: clk.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	lowered := limits.Default()
	lowered.ResponseBytes = 64
	lowered.ResultBytes = 64
	lowered.EventBytes = 64
	lowered.MaxEvents = 1
	lowered.EventsBytes = 128
	lowered.PayloadRetention = time.Minute
	lowered.TombstoneRetention = 2 * time.Minute
	s, err = store.Open(ctx, path, keys, store.Config{Limits: lowered, Now: clk.Now})
	if err != nil {
		t.Fatalf("reopen with lowered limits: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	got, err := s.AppendEvents(ctx, "sub-1", []contract.Event{
		testEvent("sub-1", 1),
		testEvent("sub-1", 2),
	})
	if err != nil {
		t.Fatalf("AppendEvents under accepted limits: %v", err)
	}
	if len(got.Events) != 2 {
		t.Fatalf("retained events = %d, want 2 under accepted limits", len(got.Events))
	}
	if got.AcceptedLimits.ResponseBytes != accepted.ResponseBytes ||
		got.AcceptedLimits.ResultBytes != accepted.ResultBytes {
		t.Fatalf("accepted limits = %+v, want original profile limits", got.AcceptedLimits)
	}

	for _, state := range []submission.State{contract.StatusQueued, contract.StatusRunning} {
		if _, err := s.Transition(ctx, "sub-1", state, store.TransitionDetail{}); err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}
	result := contract.Result{Content: []contract.ContentBlock{[]byte(
		`{"type":"text","text":"` + strings.Repeat("x", 256) + `"}`,
	)}}
	if _, err := s.Complete(ctx, "sub-1", result); err != nil {
		t.Fatalf("Complete under accepted result limit: %v", err)
	}

	clk.Advance(3 * time.Minute)
	summary, err := s.GC(ctx)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if summary.Expired != 0 || summary.Deleted != 0 {
		t.Fatalf("GC summary = %+v, want accepted retention policy", summary)
	}
	got, err = s.GetSubmission(ctx, "sub-1")
	if err != nil || got.Result == nil {
		t.Fatalf("retained result = %+v, %v", got, err)
	}
}
