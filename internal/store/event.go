package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/submission"
)

// AppendEvents appends normalized progress events, validating the strictly
// increasing sequence invariant. Retention keeps at most MaxEvents events
// and EventsBytes total; overflow drops the oldest events first.
func (s *Store) AppendEvents(ctx context.Context, id string, events []contract.Event) (*Submission, error) {
	if len(events) == 0 {
		return s.GetSubmission(ctx, id)
	}

	tx, err := s.beginWriteTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin event append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	sub, err := s.loadEncrypted(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := s.decryptSubmission(sub); err != nil {
		return nil, err
	}
	if submission.Terminal(sub.Status) {
		return nil, fmt.Errorf("append events in terminal state %s", sub.Status)
	}
	merged, last, err := mergeEvents(sub, events, sub.AcceptedLimits.eventLimits())
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("encode events: %w", err)
	}
	sealed, err := s.cipher.seal(encoded, id, "events")
	if err != nil {
		return nil, fmt.Errorf("seal events: %w", err)
	}
	nowMs := s.now().UnixMilli()
	result, err := tx.ExecContext(ctx,
		"UPDATE submissions SET events_enc = ?, sequence = ?, updated_at = ? WHERE submission_id = ? AND status = ? AND sequence = ?",
		sealed, last, nowMs, id, string(sub.Status), sub.Sequence,
	)
	if err != nil {
		return nil, fmt.Errorf("store events: %w", err)
	}
	if err := requireUpdated(result); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit events: %w", err)
	}

	sub.Events = merged
	sub.Sequence = last
	sub.UpdatedAt = time.UnixMilli(nowMs).UTC()
	return sub, nil
}

func mergeEvents(sub *Submission, events []contract.Event, lim limits.Limits) ([]contract.Event, int64, error) {
	merged := append([]contract.Event(nil), sub.Events...)
	last := sub.Sequence
	for _, event := range events {
		if event.SubmissionID != sub.ID {
			return nil, last, fmt.Errorf("event for submission %q stored under %q", event.SubmissionID, sub.ID)
		}
		if err := submission.CheckSequence(event.Sequence, last); err != nil {
			return nil, last, err
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil, last, fmt.Errorf("encode event %d: %w", event.Sequence, err)
		}
		if int64(len(encoded)) > int64(lim.EventBytes) || int64(len(encoded)) > int64(lim.EventsBytes) {
			return nil, last, fmt.Errorf("event %d exceeds retention bounds", event.Sequence)
		}
		merged = append(merged, event)
		// Retain the bound after each append so one large upstream batch does
		// not create an equally large duplicate slice before the final trim.
		merged = trimEvents(merged, lim)
		last = event.Sequence
	}
	return merged, last, nil
}

// trimEvents drops the oldest events until the count and byte bounds hold.
func trimEvents(events []contract.Event, lim limits.Limits) []contract.Event {
	for len(events) > lim.MaxEvents {
		events = events[1:]
	}
	for len(events) > 0 && encodedEventsSize(events) > int64(lim.EventsBytes) {
		events = events[1:]
	}
	return events
}

func encodedEventsSize(events []contract.Event) int64 {
	// The stored plaintext is one JSON array, so include its brackets and the
	// commas between entries rather than summing only the element encodings.
	total := int64(2)
	for index, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			continue
		}
		if index > 0 {
			total++
		}
		total += int64(len(encoded))
	}
	return total
}
