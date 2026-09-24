package application

import (
	"context"
	"strings"
	"testing"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/profile"
)

func TestDownstreamRejectsRequestErrorsAndUnapprovedOperations(t *testing.T) {
	t.Parallel()

	session := connectDownstream(t, newFakeTama(t))
	cases := []struct {
		name string
		tool string
		body string
		code contract.Code
	}{
		{
			name: "unknown await field",
			tool: contract.ToolAwait,
			body: `{"submission_id":"sub","extra":1}`,
			code: contract.CodeInvalidRequest,
		},
		{
			name: "guarded operation",
			tool: contract.ToolSubmit,
			body: `{"tool":"guarded","client_request_id":"g-1","arguments":{"note":"x"}}`,
			code: contract.CodeOperationNotAllowed,
		},
		{
			name: "unsupported operation",
			tool: contract.ToolSubmit,
			body: `{"tool":"unstable","client_request_id":"u-1","arguments":{"note":"x"}}`,
			code: contract.CodeOperationNotAllowed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := callDownstream(t, session, tc.tool, tc.body)
			if !result.IsError || downstreamCode(t, result) != tc.code {
				t.Fatalf("result = %v", result.StructuredContent)
			}
		})
	}
}

func TestDownstreamCursorBeyondSequenceIsRejected(t *testing.T) {
	t.Parallel()

	session := connectDownstream(t, newFakeTama(t))
	accepted := callDownstream(t, session, contract.ToolSubmit, systemSubmitBody("cursor-1", "unit"))
	var submitted contract.SubmitOutput
	decodeDownstream(t, accepted, &submitted)
	_ = awaitDownstream(t, session, submitted.SubmissionID, 5000, "")

	beyond := callDownstream(t, session, contract.ToolAwait, awaitBody(submitted.SubmissionID, 1, "999999", ""))
	if !beyond.IsError || downstreamCode(t, beyond) != contract.CodeInvalidRequest {
		t.Fatalf("beyond cursor = %v", beyond.StructuredContent)
	}
	negative := callDownstream(t, session, contract.ToolAwait, awaitBody(submitted.SubmissionID, -1, "", ""))
	if !negative.IsError || downstreamCode(t, negative) != contract.CodeInvalidRequest {
		t.Fatalf("negative timeout = %v", negative.StructuredContent)
	}
}

func TestDownstreamProfileAdvertisesOnlyItsOperations(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	cfg := fixtureConfigWith(t, f, limits.Default(), func(p *profile.Profile) {
		kept := p.Operations[:0]
		for _, op := range p.Operations {
			if op.Strategy == catalog.StrategyLocalReplayable {
				kept = append(kept, op)
			}
		}
		p.Operations = kept
	})
	svc, _, _ := appFromConfig(t, cfg)
	session := connectApp(t, svc)

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == contract.ToolSubmit && strings.Contains(tool.Description, "message") {
			t.Fatalf("system profile advertised the app operation: %q", tool.Description)
		}
		if tool.Name == contract.ToolSubmit && !strings.Contains(tool.Description, "status") {
			t.Fatalf("system profile omitted its operation: %q", tool.Description)
		}
	}
	rejected := callDownstream(t, session, contract.ToolSubmit, `{"tool":"message","client_request_id":"app-on-system","arguments":{"message":"hello"}}`)
	if !rejected.IsError || downstreamCode(t, rejected) != contract.CodeOperationNotAllowed {
		t.Fatalf("app tool on system profile = %v", rejected.StructuredContent)
	}
}

func TestDownstreamAppProfileRejectsSystemOperations(t *testing.T) {
	t.Parallel()

	session := connectTaskDownstream(t, newTaskUpstream(t))
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name == contract.ToolSubmit && strings.Contains(tool.Description, "status") {
			t.Fatalf("app profile advertised the system operation: %q", tool.Description)
		}
		if tool.Name == contract.ToolSubmit && !strings.Contains(tool.Description, "message") {
			t.Fatalf("app profile omitted its operation: %q", tool.Description)
		}
	}
	rejected := callDownstream(t, session, contract.ToolSubmit, systemSubmitBody("sys-on-app", "unit"))
	if !rejected.IsError || downstreamCode(t, rejected) != contract.CodeOperationNotAllowed {
		t.Fatalf("system tool on app profile = %v", rejected.StructuredContent)
	}
}
