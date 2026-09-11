// Package limits defines Tama Link's size, timeout, and retention bounds.
//
// Profiles start from the version 1 defaults. A profile may lower any bound
// and may raise one only with an explicit value at or below the
// implementation hard ceiling and a reconciled profile digest.
package limits

import (
	"fmt"
	"time"
)

// Bytes is a byte size bound.
type Bytes int64

// Common byte size units.
const (
	KiB Bytes = 1024
	MiB Bytes = 1024 * KiB
)

// Limits holds the effective bounds for one profile.
type Limits struct {
	// ArgumentsBytes bounds one canonicalized argument document.
	ArgumentsBytes Bytes `json:"arguments_bytes"`
	// ArgumentDepth bounds canonical argument nesting.
	ArgumentDepth int `json:"argument_depth"`
	// ResponseBytes bounds one upstream response body.
	ResponseBytes Bytes `json:"response_bytes"`
	// ResultBytes bounds one stored normalized terminal result.
	ResultBytes Bytes `json:"result_bytes"`
	// EventBytes bounds one progress event.
	EventBytes Bytes `json:"event_bytes"`
	// MaxEvents bounds retained progress events per submission.
	MaxEvents int `json:"max_events"`
	// EventsBytes bounds total retained event data per submission.
	EventsBytes Bytes `json:"events_bytes"`
	// AwaitDefault is the default long-poll duration.
	AwaitDefault time.Duration `json:"await_default"`
	// AwaitMax bounds one long-poll duration.
	AwaitMax time.Duration `json:"await_max"`
	// PayloadRetention bounds how long terminal payloads are retained after
	// completion.
	PayloadRetention time.Duration `json:"payload_retention"`
	// TombstoneRetention bounds how long payload-free expiry tombstones are
	// retained after completion. It must cover the payload retention.
	TombstoneRetention time.Duration `json:"tombstone_retention"`
}

// Default returns the version 1 default bounds.
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

// HardCeiling returns the implementation ceilings. Profile values may never
// exceed them.
func HardCeiling() Limits {
	return Limits{
		ArgumentsBytes:     4 * MiB,
		ArgumentDepth:      64,
		ResponseBytes:      64 * MiB,
		ResultBytes:        32 * MiB,
		EventBytes:         64 * KiB,
		MaxEvents:          512,
		EventsBytes:        4 * MiB,
		AwaitDefault:       30 * time.Second,
		AwaitMax:           60 * time.Second,
		PayloadRetention:   30 * 24 * time.Hour,
		TombstoneRetention: 90 * 24 * time.Hour,
	}
}

// Validate reports whether l is within the hard ceilings and internally
// consistent.
func (l Limits) Validate() error {
	ceilings := HardCeiling()
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
	if l.TombstoneRetention < l.PayloadRetention {
		return fmt.Errorf("tombstone_retention %s is below payload_retention %s", l.TombstoneRetention, l.PayloadRetention)
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
