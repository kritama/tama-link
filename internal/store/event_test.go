package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/submission"
)

func testEvent(submissionID string, sequence int64) contract.Event {
	return contract.Event{
		SubmissionID: submissionID,
		Sequence:     sequence,
		Timestamp:    time.Now().UTC(),
		State:        contract.StatusRunning,
		Message:      "working",
	}
}

func TestAppendEventsRoundTrip(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}

	got, err := s.AppendEvents(ctx, "sub-1", []contract.Event{
		testEvent("sub-1", 1),
		testEvent("sub-1", 2),
	})
	if err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	if got.Sequence != 2 {
		t.Fatalf("sequence = %d, want 2", got.Sequence)
	}

	fetched, err := s.GetSubmission(ctx, "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if len(fetched.Events) != 2 || fetched.Events[1].Sequence != 2 {
		t.Fatalf("events = %+v, want two events through the encrypted blob", fetched.Events)
	}
}

func TestAppendEventsEnforcesSequence(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	if _, err := s.AppendEvents(ctx, "sub-1", []contract.Event{testEvent("sub-1", 1)}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}

	tests := []struct {
		name string
		seq  int64
	}{
		{"duplicate sequence", 1},
		{"decreasing sequence", 0},
		{"below first sequence", -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if _, err := s.AppendEvents(ctx, "sub-1", []contract.Event{testEvent("sub-1", test.seq)}); err == nil {
				t.Fatalf("sequence %d accepted, want error", test.seq)
			}
		})
	}
}

func TestAppendEventsRejectsForeignSubmission(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	if _, err := s.AppendEvents(ctx, "sub-1", []contract.Event{testEvent("sub-2", 1)}); err == nil {
		t.Fatal("event for another submission accepted, want error")
	}
}

func TestAppendEventsHonoursRetention(t *testing.T) {
	keys, clk := newMemKeys(), newClock()
	lim := limits.Default()
	lim.MaxEvents = 3
	lim.EventsBytes = 1024

	path := t.TempDir() + "/state.db"
	s, err := store.Open(context.Background(), path, keys, store.Config{Limits: lim, Now: clk.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	events := make([]contract.Event, 0, 5)
	for seq := int64(1); seq <= 5; seq++ {
		events = append(events, testEvent("sub-1", seq))
	}
	got, err := s.AppendEvents(ctx, "sub-1", events)
	if err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	if len(got.Events) != 3 {
		t.Fatalf("retained events = %d, want the newest 3", len(got.Events))
	}
	if got.Events[0].Sequence != 3 || got.Events[2].Sequence != 5 {
		t.Fatalf("retained sequences = %+v, want 3 through 5", got.Events)
	}
}

func TestCaptureResultRoundTrip(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	for _, state := range []submission.State{
		submission.State(contract.StatusQueued),
		submission.State(contract.StatusRunning),
		submission.State(contract.StatusCompleted),
	} {
		if _, err := s.Transition(ctx, "sub-1", state, store.TransitionDetail{}); err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}

	result := contract.Result{
		Content:           []contract.ContentBlock{{Type: "text", Text: "done"}},
		StructuredContent: []byte(`{"ok":true}`),
	}
	if _, err := s.CaptureResult(ctx, "sub-1", result); err != nil {
		t.Fatalf("CaptureResult: %v", err)
	}

	got, err := s.GetSubmission(ctx, "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if got.Result == nil || got.Result.Content[0].Text != "done" ||
		string(got.Result.StructuredContent) != `{"ok":true}` {
		t.Fatalf("result = %+v, want round-trip through the encrypted blob", got.Result)
	}
}

func TestCaptureResultRequiresTerminalState(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	result := contract.Result{Content: []contract.ContentBlock{{Type: "text", Text: "early"}}}
	if _, err := s.CaptureResult(ctx, "sub-1", result); err == nil {
		t.Fatal("result captured in a non-terminal state, want error")
	}
}

func TestCaptureResultEnforcesSizeBound(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	lim := limits.Default()
	lim.ResultBytes = 64

	path := t.TempDir() + "/state.db"
	s, err := store.Open(context.Background(), path, keys, store.Config{Limits: lim, Now: clk.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	for _, state := range []submission.State{
		submission.State(contract.StatusQueued),
		submission.State(contract.StatusRunning),
		submission.State(contract.StatusCompleted),
	} {
		if _, err := s.Transition(ctx, "sub-1", state, store.TransitionDetail{}); err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}

	big := contract.Result{Content: []contract.ContentBlock{{Type: "text", Text: strings.Repeat("x", 128)}}}
	_, err = s.CaptureResult(ctx, "sub-1", big)
	if !errors.Is(err, store.ErrResultTooLarge) {
		t.Fatalf("oversized result = %v, want ErrResultTooLarge", err)
	}
}
