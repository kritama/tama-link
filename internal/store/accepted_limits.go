package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kritama/tama-link/internal/limits"
)

// AcceptedLimits is the lifecycle policy captured when a submission is
// accepted. Later profile changes apply only to new submissions.
type AcceptedLimits struct {
	ResponseBytes      limits.Bytes
	ResultBytes        limits.Bytes
	EventBytes         limits.Bytes
	MaxEvents          int
	EventsBytes        limits.Bytes
	PayloadRetention   time.Duration
	TombstoneRetention time.Duration
}

func acceptedLimitsFrom(lim limits.Limits) AcceptedLimits {
	return AcceptedLimits{
		ResponseBytes:      lim.ResponseBytes,
		ResultBytes:        lim.ResultBytes,
		EventBytes:         lim.EventBytes,
		MaxEvents:          lim.MaxEvents,
		EventsBytes:        lim.EventsBytes,
		PayloadRetention:   persistedDuration(lim.PayloadRetention),
		TombstoneRetention: persistedDuration(lim.TombstoneRetention),
	}
}

// SQLite timestamps are milliseconds. Round positive durations up so a valid
// sub-millisecond policy never becomes zero or expires early after a restart.
func persistedDuration(duration time.Duration) time.Duration {
	return ((duration + time.Millisecond - 1) / time.Millisecond) * time.Millisecond
}

func (lim AcceptedLimits) validate() error {
	effective := limits.Default()
	effective.ResponseBytes = lim.ResponseBytes
	effective.ResultBytes = lim.ResultBytes
	effective.EventBytes = lim.EventBytes
	effective.MaxEvents = lim.MaxEvents
	effective.EventsBytes = lim.EventsBytes
	effective.PayloadRetention = lim.PayloadRetention
	effective.TombstoneRetention = lim.TombstoneRetention
	if err := effective.Validate(); err != nil {
		return fmt.Errorf("accepted lifecycle limits: %w", err)
	}
	return nil
}

func (lim AcceptedLimits) eventLimits() limits.Limits {
	return limits.Limits{
		EventBytes:  lim.EventBytes,
		MaxEvents:   lim.MaxEvents,
		EventsBytes: lim.EventsBytes,
	}
}

func (s *Store) loadAcceptedLimits(ctx context.Context, id string) (AcceptedLimits, error) {
	var lim AcceptedLimits
	var payloadRetentionMs, tombstoneRetentionMs int64
	err := s.db.QueryRowContext(ctx, `
		SELECT response_bytes, result_bytes, event_bytes, max_events, events_bytes,
		       payload_retention_ms, tombstone_retention_ms
		FROM submissions WHERE submission_id = ?`, id).Scan(
		&lim.ResponseBytes, &lim.ResultBytes, &lim.EventBytes, &lim.MaxEvents, &lim.EventsBytes,
		&payloadRetentionMs, &tombstoneRetentionMs,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return AcceptedLimits{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return AcceptedLimits{}, fmt.Errorf("read accepted lifecycle limits: %w", err)
	}
	lim.PayloadRetention = time.Duration(payloadRetentionMs) * time.Millisecond
	lim.TombstoneRetention = time.Duration(tombstoneRetentionMs) * time.Millisecond
	if err := lim.validate(); err != nil {
		return AcceptedLimits{}, fmt.Errorf("%w: submission %s: %w", ErrStateUnavailable, id, err)
	}
	return lim, nil
}
