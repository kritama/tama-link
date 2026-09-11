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

func TestValidateAcceptsLoweredValues(t *testing.T) {
	t.Parallel()

	l := Default()
	l.ArgumentsBytes = 32 * KiB
	l.ArgumentDepth = 4
	l.MaxEvents = 16
	l.AwaitDefault = 5 * time.Second
	l.PayloadRetention = 24 * time.Hour
	if err := l.Validate(); err != nil {
		t.Fatalf("lowered limits invalid: %v", err)
	}
}

func TestValidateRejectsExcessiveValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Limits)
	}{
		{"zero arguments bytes", func(l *Limits) { l.ArgumentsBytes = 0 }},
		{"above arguments ceiling", func(l *Limits) { l.ArgumentsBytes = 2 * MiB }},
		{"zero argument depth", func(l *Limits) { l.ArgumentDepth = 0 }},
		{"above depth ceiling", func(l *Limits) { l.ArgumentDepth = 33 }},
		{"above response ceiling", func(l *Limits) { l.ResponseBytes = 17 * MiB }},
		{"above result ceiling", func(l *Limits) { l.ResultBytes = 9 * MiB }},
		{"above event ceiling", func(l *Limits) { l.EventBytes = 17 * KiB }},
		{"above max events", func(l *Limits) { l.MaxEvents = 129 }},
		{"above event data ceiling", func(l *Limits) { l.EventsBytes = 2 * MiB }},
		{"zero await default", func(l *Limits) { l.AwaitDefault = 0 }},
		{"above await max", func(l *Limits) { l.AwaitMax = 31 * time.Second }},
		{"default above max", func(l *Limits) { l.AwaitDefault = 20 * time.Second; l.AwaitMax = 15 * time.Second }},
		{"above payload retention", func(l *Limits) { l.PayloadRetention = 8 * 24 * time.Hour }},
		{"above tombstone retention", func(l *Limits) { l.TombstoneRetention = 31 * 24 * time.Hour }},
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
