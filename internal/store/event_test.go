package store_test

import (
	"context"
	"encoding/json"
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
	clk.Advance(time.Minute)

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
	if got.UpdatedAt != clk.Now() {
		t.Fatalf("updated_at = %s, want %s", got.UpdatedAt, clk.Now())
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

func TestAppendEventsCountsStoredArrayOverhead(t *testing.T) {
	keys, clk := newMemKeys(), newClock()
	event := testEvent("sub-1", 1)
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal event: %v", err)
	}
	lim := limits.Default()
	lim.EventsBytes = limits.Bytes(len(encoded) + 1)

	s, err := store.Open(context.Background(), t.TempDir()+"/state.db", keys, store.Config{Limits: lim, Now: clk.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.CreateSubmission(context.Background(), testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}

	got, err := s.AppendEvents(context.Background(), "sub-1", []contract.Event{event})
	if err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	if got.Sequence != 1 || len(got.Events) != 0 {
		t.Fatalf("retained event state = sequence %d, events %d; want sequence 1 and no oversized array", got.Sequence, len(got.Events))
	}
}

func TestAppendEventsKeepsSequenceAfterTrimming(t *testing.T) {
	keys, clk := newMemKeys(), newClock()
	lim := limits.Default()
	lim.MaxEvents = 1
	s, err := store.Open(context.Background(), t.TempDir()+"/state.db", keys, store.Config{Limits: lim, Now: clk.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.CreateSubmission(context.Background(), testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	if _, err := s.AppendEvents(context.Background(), "sub-1", []contract.Event{
		testEvent("sub-1", 1), testEvent("sub-1", 2),
	}); err != nil {
		t.Fatalf("AppendEvents: %v", err)
	}
	if _, err := s.AppendEvents(context.Background(), "sub-1", []contract.Event{testEvent("sub-1", 2)}); err == nil {
		t.Fatal("trimmed sequence accepted again")
	}
}

func TestAppendEventsRejectsOversizedAndTerminalEvents(t *testing.T) {
	keys, clk := newMemKeys(), newClock()
	lim := limits.Default()
	lim.EventBytes = 128
	lim.EventsBytes = 128
	s, err := store.Open(context.Background(), t.TempDir()+"/state.db", keys, store.Config{Limits: lim, Now: clk.Now})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	oversized := testEvent("sub-1", 1)
	oversized.Message = strings.Repeat("x", 256)
	if _, err := s.AppendEvents(ctx, "sub-1", []contract.Event{oversized}); err == nil {
		t.Fatal("oversized event accepted")
	}
	for _, state := range []submission.State{contract.StatusQueued, contract.StatusRunning} {
		if _, err := s.Transition(ctx, "sub-1", state, store.TransitionDetail{}); err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}
	if _, err := s.Complete(ctx, "sub-1", contract.Result{Content: []contract.ContentBlock{[]byte(`{"type":"text","text":"done"}`)}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := s.AppendEvents(ctx, "sub-1", []contract.Event{testEvent("sub-1", 1)}); err == nil {
		t.Fatal("event appended after terminal completion")
	}
}

func TestCompleteRoundTrip(t *testing.T) {
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
	} {
		if _, err := s.Transition(ctx, "sub-1", state, store.TransitionDetail{}); err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}

	result := contract.Result{
		Content:           []contract.ContentBlock{[]byte(`{"type":"text","text":"done"}`)},
		StructuredContent: []byte(`{"ok":true}`),
	}
	if _, err := s.Complete(ctx, "sub-1", result); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, err := s.GetSubmission(ctx, "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if got.Result == nil || string(got.Result.Content[0]) != `{"type":"text","text":"done"}` ||
		string(got.Result.StructuredContent) != `{"ok":true}` {
		t.Fatalf("result = %+v, want round-trip through the encrypted blob", got.Result)
	}
	other := contract.Result{Content: []contract.ContentBlock{[]byte(`{"type":"text","text":"other"}`)}}
	if _, err := s.Complete(ctx, "sub-1", other); err == nil {
		t.Fatal("second completion overwrote terminal result")
	}
	stable, err := s.GetSubmission(ctx, "sub-1")
	if err != nil || string(stable.Result.Content[0]) != `{"type":"text","text":"done"}` {
		t.Fatalf("stable result = %+v, %v", stable.Result, err)
	}
}

func TestCompleteRequiresRunningState(t *testing.T) {
	t.Parallel()

	keys, clk := newMemKeys(), newClock()
	s, _ := openTestStore(t, keys, clk)
	ctx := context.Background()

	if _, err := s.CreateSubmission(ctx, testSubmission("sub-1", "req-1")); err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
	result := contract.Result{Content: []contract.ContentBlock{[]byte(`{"type":"text","text":"early"}`)}}
	if _, err := s.Complete(ctx, "sub-1", result); err == nil {
		t.Fatal("result captured before running, want error")
	}
}

func TestCompleteEnforcesSizeBound(t *testing.T) {
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
	} {
		if _, err := s.Transition(ctx, "sub-1", state, store.TransitionDetail{}); err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}

	big := contract.Result{Content: []contract.ContentBlock{[]byte(`{"type":"text","text":"` + strings.Repeat("x", 128) + `"}`)}}
	_, err = s.Complete(ctx, "sub-1", big)
	if !errors.Is(err, store.ErrResultTooLarge) {
		t.Fatalf("oversized result = %v, want ErrResultTooLarge", err)
	}
	got, getErr := s.GetSubmission(ctx, "sub-1")
	if getErr != nil {
		t.Fatalf("GetSubmission: %v", getErr)
	}
	if got.Status != contract.StatusFailed || got.ErrorCode != string(contract.CodeResultTooLarge) || got.Result != nil {
		t.Fatalf("oversized terminal state = %+v, want failed result_too_large without result", got)
	}
}
