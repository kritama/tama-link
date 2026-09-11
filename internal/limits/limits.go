// Package limits defines Tama Link's size, timeout, and retention bounds.
//
// Profiles may lower the version 1 defaults; a value may never exceed the
// default, which is the implementation hard ceiling.
package limits

import (
	"fmt"
	"time"
)

// Bytes is a byte size bound.
type Bytes int64

const (
	KiB Bytes = 1024
	MiB Bytes = 1024 * KiB
)

// Limits holds the effective bounds for one profile.
type Limits struct {
	// ArgumentsBytes bounds one canonicalized argument document.
	ArgumentsBytes Bytes
	// ArgumentDepth bounds canonical argument nesting.
	ArgumentDepth int
	// ResponseBytes bounds one upstream response body.
	ResponseBytes Bytes
	// ResultBytes bounds one stored normalized terminal result.
	ResultBytes Bytes
	// EventBytes bounds one progress event.
	EventBytes Bytes
	// MaxEvents bounds retained progress events per submission.
	MaxEvents int
	// EventsBytes bounds total retained event data per submission.
	EventsBytes Bytes
	// AwaitDefault is the default long-poll duration.
	AwaitDefault time.Duration
	// AwaitMax bounds one long-poll duration.
	AwaitMax time.Duration
	// PayloadRetention bounds how long terminal payloads are retained.
	PayloadRetention time.Duration
	// TombstoneRetention bounds how long payload-free expiry tombstones are
	// retained after completion.
	TombstoneRetention time.Duration
}

// Default returns the version 1 default bounds, which are also the hard
// ceilings.
func Default() Limits {
	return Limits{
		ArgumentsBytes:     MiB,
		ArgumentDepth:      32,
		ResponseBytes:      16 * MiB,
		ResultBytes:        8 * MiB,
		EventBytes:         16 * KiB,
		MaxEvents:          128,
		EventsBytes:        MiB,
		AwaitDefault:       20 * time.Second,
		AwaitMax:           30 * time.Second,
		PayloadRetention:   7 * 24 * time.Hour,
		TombstoneRetention: 30 * 24 * time.Hour,
	}
}

// Validate reports whether l is within the hard ceilings.
func (l Limits) Validate() error {
	ceilings := Default()
	if err := checkBytes("arguments_bytes", l.ArgumentsBytes, ceilings.ArgumentsBytes); err != nil {
		return err
	}
	if err := checkInt("argument_depth", l.ArgumentDepth, ceilings.ArgumentDepth); err != nil {
		return err
	}
	if err := checkBytes("response_bytes", l.ResponseBytes, ceilings.ResponseBytes); err != nil {
		return err
	}
	if err := checkBytes("result_bytes", l.ResultBytes, ceilings.ResultBytes); err != nil {
		return err
	}
	if err := checkBytes("event_bytes", l.EventBytes, ceilings.EventBytes); err != nil {
		return err
	}
	if err := checkInt("max_events", l.MaxEvents, ceilings.MaxEvents); err != nil {
		return err
	}
	if err := checkBytes("events_bytes", l.EventsBytes, ceilings.EventsBytes); err != nil {
		return err
	}
	if err := checkDuration("await_default", l.AwaitDefault, ceilings.AwaitDefault); err != nil {
		return err
	}
	if err := checkDuration("await_max", l.AwaitMax, ceilings.AwaitMax); err != nil {
		return err
	}
	if l.AwaitDefault > l.AwaitMax {
		return fmt.Errorf("await_default %s exceeds await_max %s", l.AwaitDefault, l.AwaitMax)
	}
	if err := checkDuration("payload_retention", l.PayloadRetention, ceilings.PayloadRetention); err != nil {
		return err
	}
	if err := checkDuration("tombstone_retention", l.TombstoneRetention, ceilings.TombstoneRetention); err != nil {
		return err
	}
	return nil
}

func checkBytes(name string, got Bytes, ceiling Bytes) error {
	if got <= 0 || got > ceiling {
		return fmt.Errorf("%s %d is outside (0, %d]", name, got, ceiling)
	}
	return nil
}

func checkInt(name string, got, ceiling int) error {
	if got <= 0 || got > ceiling {
		return fmt.Errorf("%s %d is outside (0, %d]", name, got, ceiling)
	}
	return nil
}

func checkDuration(name string, got, ceiling time.Duration) error {
	if got <= 0 || got > ceiling {
		return fmt.Errorf("%s %s is outside (0, %s]", name, got, ceiling)
	}
	return nil
}
