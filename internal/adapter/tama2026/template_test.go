package tama2026

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/upstream"
)

func TestTemplatesMaterializeReviewedDescriptors(t *testing.T) {
	t.Parallel()
	for _, kind := range []profile.Kind{profile.KindApp, profile.KindSystem} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			tmpl, err := Lookup(kind)
			if err != nil {
				t.Fatal(err)
			}
			live := liveTools(tmpl)
			// Reverse live order and add an unapproved tool. Neither may
			// change the published descriptors.
			reversed := append([]*upstream.LiveTool{{
				Name:        "unapproved.extra",
				InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
			}}, live...)
			for i, j := 1, len(reversed)-1; i < j; i, j = i+1, j-1 {
				reversed[i], reversed[j] = reversed[j], reversed[i]
			}
			ops, err := tmpl.Materialize(discoverFor(tmpl), reversed)
			if err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			if len(ops) != len(tmpl.Operations) {
				t.Fatalf("operations = %d, want %d", len(ops), len(tmpl.Operations))
			}
			for i := range ops {
				if ops[i].Name != tmpl.Operations[i].Name || ops[i].Digest != tmpl.Operations[i].Digest {
					t.Fatalf("descriptor %d = %s %s", i, ops[i].Name, ops[i].Digest)
				}
				if ops[i].Strategy != tmpl.Operations[i].Strategy {
					t.Fatalf("strategy for %s changed", ops[i].Name)
				}
			}
		})
	}
}

func TestSystemReadIgnoresScopeHiddenReviewTool(t *testing.T) {
	t.Parallel()
	tmpl, err := Lookup(profile.KindSystem)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range tmpl.Scopes {
		if scope == "mcp.reflection.review" {
			t.Fatal("system-read bundle requests the review scope")
		}
	}
	for _, op := range tmpl.Operations {
		if op.Name == "reflection.comments.review" {
			t.Fatal("system-read template requires a scope-hidden tool")
		}
	}
	// The live catalog is what a token granted only the template scopes can
	// see. The review mutation is advertised but filtered out of that view,
	// and must not be required or enabled.
	live := liveTools(tmpl)
	live = append(live, &upstream.LiveTool{
		Name:        "reflection.comments.review",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":true,"properties":{"comment_id":{"type":"string"}}}`),
	})
	ops, err := tmpl.Materialize(discoverFor(tmpl), live)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	for _, op := range ops {
		if op.Name == "reflection.comments.review" {
			t.Fatal("published the scope-hidden review tool")
		}
	}
	if len(ops) != len(tmpl.Operations) {
		t.Fatalf("operations = %d, want %d", len(ops), len(tmpl.Operations))
	}
}

func TestMaterializeRejectsMissingApprovedTool(t *testing.T) {
	t.Parallel()
	tmpl, err := Lookup(profile.KindApp)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tmpl.Materialize(discoverFor(tmpl), nil)
	if err == nil || !strings.Contains(err.Error(), "not live") {
		t.Fatalf("error = %v", err)
	}
}

func discoverFor(tmpl Template) *upstream.DiscoverResult {
	caps := `{"tools":{}}`
	if tmpl.RequiresTasks {
		caps = `{"tools":{},"extensions":{"io.modelcontextprotocol/tasks":{}}}`
	}
	return &upstream.DiscoverResult{
		SupportedVersions: []string{protocolVersion},
		Capabilities:      json.RawMessage(caps),
		Instructions:      tmpl.Instructions,
	}
}

func liveTools(tmpl Template) []*upstream.LiveTool {
	out := make([]*upstream.LiveTool, 0, len(tmpl.Operations))
	for _, op := range tmpl.Operations {
		annotations, err := json.Marshal(op.Annotations)
		if err != nil {
			panic(err)
		}
		out = append(out, &upstream.LiveTool{
			Name:         op.Name,
			InputSchema:  op.InputSchema,
			OutputSchema: op.OutputSchema,
			Annotations:  annotations,
		})
	}
	return out
}
