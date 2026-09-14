package limits

import (
	"testing"
	"time"
)

func TestDefaultMatchesSpec(t *testing.T) {
	t.Parallel()

	l := Default()
	if err := l.Validate(); err != nil {
		t.Fatalf("default limits invalid: %v", err)
	}

	want := Limits{
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
	if l != want {
		t.Fatalf("defaults = %+v, want %+v", l, want)
	}
}

func TestHardCeilingCoversDefaults(t *testing.T) {
	t.Parallel()

	ceilings := HardCeiling()
	if err := ceilings.Validate(); err != nil {
		t.Fatalf("hard ceilings invalid: %v", err)
	}
	defaults := Default()
	if ceilings.ArgumentsBytes < defaults.ArgumentsBytes ||
		ceilings.ArgumentDepth < defaults.ArgumentDepth ||
		ceilings.ResponseBytes < defaults.ResponseBytes ||
		ceilings.ResultBytes < defaults.ResultBytes ||
		ceilings.EventBytes < defaults.EventBytes ||
		ceilings.MaxEvents < defaults.MaxEvents ||
		ceilings.EventsBytes < defaults.EventsBytes ||
		ceilings.AwaitDefault < defaults.AwaitDefault ||
		ceilings.AwaitMax < defaults.AwaitMax ||
		ceilings.PayloadRetention < defaults.PayloadRetention ||
		ceilings.TombstoneRetention < defaults.TombstoneRetention {
		t.Fatalf("hard ceilings must cover the defaults:\n%+v\n%+v", ceilings, defaults)
	}
}

func TestValidateAcceptsLoweredValues(t *testing.T) {
	t.Parallel()

	l := Default()
	l.ArgumentsBytes = 32 * KiB
	l.ArgumentDepth = 4
	l.MaxEvents = 16
	l.AwaitDefault = 5 * time.Second
	l.PayloadRetention = 24 * time.Hour
	l.TombstoneRetention = 48 * time.Hour
	if err := l.Validate(); err != nil {
		t.Fatalf("lowered limits invalid: %v", err)
	}
}

func TestValidateAcceptsRaisedValuesWithinCeilings(t *testing.T) {
	t.Parallel()

	l := Default()
	l.ArgumentsBytes = 2 * MiB
	l.ArgumentDepth = 64
	l.AwaitMax = 45 * time.Second
	l.PayloadRetention = 14 * 24 * time.Hour
	l.TombstoneRetention = 60 * 24 * time.Hour
	if err := l.Validate(); err != nil {
		t.Fatalf("raised-within-ceilings limits invalid: %v", err)
	}
}

func TestRetentionOrdering(t *testing.T) {
	t.Parallel()

	equal := Default()
	shared := 14 * 24 * time.Hour
	equal.PayloadRetention = shared
	equal.TombstoneRetention = shared
	if err := equal.Validate(); err != nil {
		t.Fatalf("equal retention invalid: %v", err)
	}

	reversed := Default()
	reversed.PayloadRetention = 30 * 24 * time.Hour
	reversed.TombstoneRetention = 14 * 24 * time.Hour
	if err := reversed.Validate(); err == nil {
		t.Fatal("tombstone retention below payload retention accepted, want error")
	}
}

func TestValidateRejectsExcessiveValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Limits)
	}{
		{"zero arguments bytes", func(l *Limits) { l.ArgumentsBytes = 0 }},
		{"above arguments ceiling", func(l *Limits) { l.ArgumentsBytes = 5 * MiB }},
		{"zero argument depth", func(l *Limits) { l.ArgumentDepth = 0 }},
		{"above depth ceiling", func(l *Limits) { l.ArgumentDepth = 65 }},
		{"above response ceiling", func(l *Limits) { l.ResponseBytes = 65 * MiB }},
		{"above result ceiling", func(l *Limits) { l.ResultBytes = 33 * MiB }},
		{"above event ceiling", func(l *Limits) { l.EventBytes = 65 * KiB }},
		{"above max events", func(l *Limits) { l.MaxEvents = 513 }},
		{"above event data ceiling", func(l *Limits) { l.EventsBytes = 5 * MiB }},
		{"zero await default", func(l *Limits) { l.AwaitDefault = 0 }},
		{"above await max ceiling", func(l *Limits) { l.AwaitMax = 61 * time.Second }},
		{"default above max", func(l *Limits) { l.AwaitDefault = 20 * time.Second; l.AwaitMax = 15 * time.Second }},
		{"above payload retention ceiling", func(l *Limits) { l.PayloadRetention = 31 * 24 * time.Hour }},
		{"above tombstone retention ceiling", func(l *Limits) { l.TombstoneRetention = 91 * 24 * time.Hour }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			l := Default()
			test.mutate(&l)
			if err := l.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want error for %s", test.name)
			}
		})
	}
}
