package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

// testDescriptor returns a structurally valid descriptor with a matching
// digest, applying each mutation before the digest is computed.
func testDescriptor(t *testing.T, mutate ...func(*Descriptor)) Descriptor {
	t.Helper()

	d := Descriptor{
		Name:        "message",
		Title:       "Send a message",
		Description: "Send one message to Tama.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"content":{"type":"string","description":"Message text"},"recipient":{"type":"string"}},"required":["content"]}`),
		TaskSupport: TaskSupportOptional,
		Strategy:    StrategyUpstreamTask,
	}
	for _, apply := range mutate {
		apply(&d)
	}
	if sum, err := d.ComputeDigest(); err == nil {
		d.Digest = sum
	}
	return d
}

func TestDigestIsCanonical(t *testing.T) {
	t.Parallel()

	a := testDescriptor(t, func(d *Descriptor) {
		d.InputSchema = json.RawMessage(`{"type":"object","properties":{"b":1,"a":1}}`)
	})
	b := testDescriptor(t, func(d *Descriptor) {
		d.InputSchema = json.RawMessage(`{"type":"object","properties":{"a":1,"b":1}}`)
	})
	if a.Digest != b.Digest {
		t.Fatalf("digests differ for equivalent schemas:\n%s\n%s", a.Digest, b.Digest)
	}
	if !strings.HasPrefix(a.Digest, "sha256:") {
		t.Fatalf("digest %q has unexpected format", a.Digest)
	}
}

func TestDigestChangesWithFields(t *testing.T) {
	t.Parallel()

	base := testDescriptor(t)
	for name, mutate := range map[string]func(*Descriptor){
		"title":       func(d *Descriptor) { d.Title = "other" },
		"description": func(d *Descriptor) { d.Description = "other" },
		"schema":      func(d *Descriptor) { d.InputSchema = json.RawMessage(`{"type":"object"}`) },
		"bindings":    func(d *Descriptor) { d.Bindings = []Binding{{Source: SourceClientRequestID, Target: "/identifier"}} },
		"strategy":    func(d *Descriptor) { d.Strategy = StrategyLocalReplayable },
		"task":        func(d *Descriptor) { d.TaskSupport = TaskSupportForbidden },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			other := testDescriptor(t, mutate)
			if other.Digest == base.Digest {
				t.Fatalf("digest unchanged after mutating %s", name)
			}
		})
	}
}

func TestTaskSupportStatesAreDistinct(t *testing.T) {
	t.Parallel()

	states := []TaskSupport{TaskSupportForbidden, TaskSupportOptional, TaskSupportRequired}
	digests := make([]string, 0, len(states))
	for _, state := range states {
		if !state.Valid() {
			t.Fatalf("TaskSupport %q is not valid", state)
		}
		digests = append(digests, testDescriptor(t, func(d *Descriptor) {
			d.TaskSupport = state
		}).Digest)
	}
	for i := 0; i < len(digests); i++ {
		for j := i + 1; j < len(digests); j++ {
			if digests[i] == digests[j] {
				t.Fatalf("task support states %s and %s share digest %s", states[i], states[j], digests[i])
			}
		}
	}
}

func TestCheckDigest(t *testing.T) {
	t.Parallel()

	d := testDescriptor(t)
	if err := d.CheckDigest(); err != nil {
		t.Fatalf("CheckDigest() = %v, want nil", err)
	}
	d.Title = "tampered"
	if err := d.CheckDigest(); err == nil {
		t.Fatal("CheckDigest() = nil after tampering, want mismatch error")
	}
}

func TestDescriptorValidate(t *testing.T) {
	t.Parallel()

	if err := testDescriptor(t).Validate(); err != nil {
		t.Fatalf("valid descriptor rejected: %v", err)
	}
	if err := testDescriptor(t, func(d *Descriptor) {
		d.OutputSchema = json.RawMessage(`{"type":"array","items":{"type":"string"}}`)
	}).Validate(); err != nil {
		t.Fatalf("array output schema rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Descriptor)
	}{
		{"empty name", func(d *Descriptor) { d.Name = "" }},
		{"long name", func(d *Descriptor) { d.Name = strings.Repeat("a", 129) }},
		{"long title", func(d *Descriptor) { d.Title = strings.Repeat("t", maxTitleBytes+1) }},
		{"long description", func(d *Descriptor) { d.Description = strings.Repeat("d", maxDescriptionBytes+1) }},
		{"unknown strategy", func(d *Descriptor) { d.Strategy = "teleport" }},
		{"missing input schema", func(d *Descriptor) { d.InputSchema = nil }},
		{"invalid schema JSON", func(d *Descriptor) { d.InputSchema = json.RawMessage(`{"type":`) }},
		{"non-object schema", func(d *Descriptor) { d.InputSchema = json.RawMessage(`["object"]`) }},
		{"non-object schema type", func(d *Descriptor) { d.InputSchema = json.RawMessage(`{"type":"string"}`) }},
		{"oversized schema", func(d *Descriptor) {
			d.InputSchema = json.RawMessage(`{"pad":"` + strings.Repeat("x", maxSchemaBytes) + `"}`)
		}},
		{"unreviewed binding", func(d *Descriptor) { d.Bindings = []Binding{{Source: "arguments.free", Target: "/x"}} }},
		{"unknown task support", func(d *Descriptor) { d.TaskSupport = "sometimes" }},
		{"trailing schema data", func(d *Descriptor) { d.InputSchema = json.RawMessage(`{"type":"object"} x`) }},
		{"duplicate schema keys", func(d *Descriptor) { d.InputSchema = json.RawMessage(`{"type":"object","type":"object"}`) }},
		{"invalid pointer escape", func(d *Descriptor) { d.Bindings = []Binding{{Source: SourceClientRequestID, Target: "/a~2"}} }},
		{"duplicate binding target", func(d *Descriptor) {
			d.Bindings = []Binding{{Source: SourceClientRequestID, Target: "/x"}, {Source: SourceClientContextThreadID, Target: "/x"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if err := testDescriptor(t, test.mutate).Validate(); err == nil {
				t.Fatalf("Validate() = nil, want error for %s", test.name)
			}
		})
	}
}

func TestBindingValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		binding Binding
		valid   bool
	}{
		{Binding{Source: SourceClientRequestID, Target: "/identifier"}, true},
		{Binding{Source: SourceClientContextThreadID, Target: "/thread/identifier", Required: true}, true},
		{Binding{Source: SourceClientRequestID, Target: "/"}, true},
		{Binding{Source: SourceClientRequestID, Target: "/~0"}, true},
		{Binding{Source: SourceClientRequestID, Target: "/a~1b"}, true},
		{Binding{Source: SourceClientRequestID, Target: "/a~0b~1"}, true},
		{Binding{Source: SourceClientRequestID, Target: "/~/a"}, false},
		{Binding{Source: "arguments.free", Target: "/x"}, false},
		{Binding{Source: "", Target: "/x"}, false},
		{Binding{Source: SourceClientRequestID, Target: "identifier"}, false},
		{Binding{Source: SourceClientRequestID, Target: ""}, false},
		{Binding{Source: SourceClientRequestID, Target: "/a~"}, false},
		{Binding{Source: SourceClientRequestID, Target: "/a~2"}, false},
		{Binding{Source: SourceClientRequestID, Target: "/a~/~"}, false},
	}
	for _, test := range tests {
		if got := test.binding.Valid(); got != test.valid {
			t.Fatalf("binding %+v Valid() = %v, want %v", test.binding, got, test.valid)
		}
	}
}
