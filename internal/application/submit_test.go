package application

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
)

func TestSubmitReplayableCompletes(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)

	id := submitStatus(t, svc, "req-1")
	if id == "" {
		t.Fatal("empty submission id")
	}

	out := awaitTerminal(t, svc, id)
	if out.Status != contract.StatusCompleted {
		t.Fatalf("status = %s, want completed", out.Status)
	}
	if out.Result == nil {
		t.Fatal("missing terminal result")
	}
	var structured struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(out.Result.StructuredContent, &structured); err != nil {
		t.Fatalf("result structured content: %v", err)
	}
	if !structured.OK {
		t.Fatalf("result = %s", string(out.Result.StructuredContent))
	}
	if f.calls.Load() != 1 {
		t.Fatalf("tools/call executed %d times, want exactly 1", f.calls.Load())
	}
}

func TestSubmitRejectedCases(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)

	cases := []struct {
		name string
		in   contract.SubmitInput
		want contract.Code
	}{
		{
			name: "unknown tool",
			in:   contract.SubmitInput{Tool: "nope", ClientRequestID: "r-1", Arguments: json.RawMessage(`{}`)},
			want: contract.CodeOperationNotAllowed,
		},
		{
			name: "arguments fail client schema",
			in: contract.SubmitInput{
				Tool: "status", ClientRequestID: "r-2",
				Arguments: json.RawMessage(`{"detail":5}`),
			},
			want: contract.CodeInvalidRequest,
		},
		{
			name: "arguments not an object",
			in:   contract.SubmitInput{Tool: "status", ClientRequestID: "r-3", Arguments: json.RawMessage(`[1]`)},
			want: contract.CodeInvalidRequest,
		},
		{
			name: "guarded operation rejected before upstream",
			in:   contract.SubmitInput{Tool: "guarded", ClientRequestID: "r-5", Arguments: json.RawMessage(`{"note":"n"}`)},
			want: contract.CodeOperationNotAllowed,
		},
		{
			name: "unsupported operation rejected before upstream",
			in:   contract.SubmitInput{Tool: "unstable", ClientRequestID: "r-6", Arguments: json.RawMessage(`{"note":"n"}`)},
			want: contract.CodeOperationNotAllowed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, appErr := svc.Submit(context.Background(), tc.in)
			if appErr == nil || appErr.Code != tc.want {
				t.Fatalf("error = %+v, want %s", appErr, tc.want)
			}
		})
	}
}

func TestSubmitTaskToolNotEnabled(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	cfg := appFixtureConfigWith(t, f, limits.Default(), nil)
	svc, st, _ := appFromConfig(t, cfg)

	out, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "message",
		ClientRequestID: "r-task",
		Arguments:       json.RawMessage(`{"message":"hi"}`),
	})
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	if out.SubmissionID == "" {
		t.Fatal("accepted task submission has no id")
	}
	sub, err := st.GetSubmission(context.Background(), out.SubmissionID)
	if err != nil {
		t.Fatalf("read accepted submission: %v", err)
	}
	if sub.Strategy != string(catalog.StrategyUpstreamTask) {
		t.Fatalf("strategy = %s", sub.Strategy)
	}
}

// TestSubmitRejectionsLeaveNoTrace proves that every strategy-gated or
// schema-rejected call creates no durable submission and claims no
// idempotency key: the same client_request_id stays free for a clean retry
// with different arguments, and the upstream is never touched.
func TestSubmitRejectionsLeaveNoTrace(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)

	rejected := []contract.SubmitInput{
		{Tool: "guarded", ClientRequestID: "clean-1", Arguments: json.RawMessage(`{"note":"n"}`)},
		{Tool: "unstable", ClientRequestID: "clean-1", Arguments: json.RawMessage(`{"note":"n"}`)},
		{Tool: "status", ClientRequestID: "clean-1", Arguments: json.RawMessage(`{"detail":5}`)},
	}
	for _, in := range rejected {
		if _, appErr := svc.Submit(context.Background(), in); appErr == nil {
			t.Fatalf("submit %s was accepted, want rejection", in.Tool)
		}
	}
	if calls := f.calls.Load(); calls != 0 {
		t.Fatalf("fixture upstream was touched: %d calls", calls)
	}

	// If any rejected call had claimed the id, this replayable resubmission
	// with different arguments would hit the idempotency conflict instead of
	// a clean acceptance.
	id := submitStatus(t, svc, "clean-1")
	awaitTerminal(t, svc, id)
}

// TestSubmitGeneratesClientRequestID pins the public contract: the key is
// optional, Tama Link generates one before durable acceptance, and each
// omission is a fresh submission rather than an idempotent retry.
func TestSubmitGeneratesClientRequestID(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)

	first, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:          "status",
		ClientContext: &contract.ClientContext{ThreadID: "thread-1"},
		Arguments:     json.RawMessage(`{"detail":"a"}`),
	})
	if appErr != nil {
		t.Fatalf("omit: %s", appErr.Message)
	}
	if first.ClientRequestID == "" || len(first.ClientRequestID) < 10 {
		t.Fatalf("generated client_request_id = %q, want a non-empty key", first.ClientRequestID)
	}

	// A second omission must not collide with the first generated key:
	// each omission is a new submission.
	second, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:          "status",
		ClientContext: &contract.ClientContext{ThreadID: "thread-2"},
		Arguments:     json.RawMessage(`{"detail":"b"}`),
	})
	if appErr != nil {
		t.Fatalf("second omission: %s", appErr.Message)
	}
	if second.SubmissionID == first.SubmissionID {
		t.Fatalf("two omissions produced one submission %s; generated keys must be fresh", first.SubmissionID)
	}
	if second.ClientRequestID == first.ClientRequestID {
		t.Fatalf("two omissions produced one key %s; generated keys must be unique", first.ClientRequestID)
	}

	// An explicit key still idempotizes on the original submission.
	run, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: first.ClientRequestID,
		ClientContext:   &contract.ClientContext{ThreadID: "thread-1"},
		Arguments:       json.RawMessage(`{"detail":"a"}`),
	})
	if appErr != nil {
		t.Fatalf("explicit retry: %s", appErr.Message)
	}
	if run.SubmissionID != first.SubmissionID {
		t.Fatalf("explicit retry returned %s, want %s", run.SubmissionID, first.SubmissionID)
	}
}

func TestSubmitIdempotency(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)

	first := submitStatus(t, svc, "dup-1")

	out, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "dup-1",
		ClientContext:   &contract.ClientContext{ThreadID: "thread-1"},
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	})
	if appErr != nil {
		t.Fatalf("retry: %s", appErr.Message)
	}
	if out.SubmissionID != first {
		t.Fatalf("retry returned %s, want %s", out.SubmissionID, first)
	}

	conflict, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "dup-1",
		Arguments:       json.RawMessage(`{"detail":"different"}`),
	})
	if appErr == nil || appErr.Code != contract.CodeIdempotencyConflict {
		t.Fatalf("conflict = %+v, want idempotency_conflict (submission %+v)", appErr, conflict)
	}
}

func TestSubmitAppliesBindings(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, _, _ := fixtureApp(t, f)
	// The fixture profile pins bindings on the status tool.
	id := submitStatus(t, svc, "bind-1")
	awaitTerminal(t, svc, id)

	f.mu.Lock()
	defer f.mu.Unlock()
	var call struct {
		Params struct {
			Arguments struct {
				Detail     string `json:"detail"`
				ClientMeta struct {
					RequestID string `json:"client_request_id"`
				} `json:"client_meta"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(f.lastCallDoc), &call); err != nil {
		t.Fatalf("last call: %v", err)
	}
	if call.Params.Arguments.Detail != "unit" {
		t.Fatalf("detail = %q", call.Params.Arguments.Detail)
	}
	if call.Params.Arguments.ClientMeta.RequestID != "bind-1" {
		t.Fatalf("binding not applied: %+v", call.Params.Arguments.ClientMeta)
	}
}

// TestSubmitIdempotentReplayDoesNotAppendEvents pins the replay contract:
// an exact client_request_id retry returns the original submission without
// appending another accepted event or re-offering the row to the worker,
// so an advanced row keeps a monotonic progress sequence.
func TestSubmitIdempotentReplayDoesNotAppendEvents(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	svc, st, _ := fixtureApp(t, f)

	first := submitStatus(t, svc, "replay-1")

	// Let the first submission complete so its row has advanced far past
	// accepted; a replay that appended an accepted event would produce a
	// sequence that descends after the terminal events.
	deadline := time.Now().Add(20 * time.Second)
	for {
		sub, err := st.GetSubmission(context.Background(), first)
		if err != nil {
			t.Fatalf("GetSubmission: %v", err)
		}
		if string(sub.Status) == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("submission did not complete: %s", sub.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	out, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "replay-1",
		ClientContext:   &contract.ClientContext{ThreadID: "thread-1"},
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	})
	if appErr != nil {
		t.Fatalf("replay: %s", appErr.Message)
	}
	if out.SubmissionID != first {
		t.Fatalf("replay returned %s, want %s", out.SubmissionID, first)
	}

	// The event stream must be unchanged by the replay: it ends in the
	// terminal completed event with strictly increasing sequences.
	// A spurious accepted append would appear as a trailing accepted
	// event after the terminal one.
	sub, err := st.GetSubmission(context.Background(), first)
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	// Replaying again must still return the same row with the same status;
	// the sequence must not have grown.
	if _, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "replay-1",
		ClientContext:   &contract.ClientContext{ThreadID: "thread-1"},
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	}); appErr != nil {
		t.Fatalf("second replay: %s", appErr.Message)
	}
	again, err := st.GetSubmission(context.Background(), first)
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if again.Sequence != sub.Sequence {
		t.Fatalf("replay advanced the sequence from %d to %d; replays must not append", sub.Sequence, again.Sequence)
	}
}

// TestSubmitReplaysBeforeCredentialCheck pins recovery of a lost submit
// response: an exact retry of an accepted client_request_id returns the
// durable submission even when credentials have since been removed. The
// replay accepts no new work, so current authentication state is
// irrelevant to it.
func TestSubmitReplaysBeforeCredentialCheck(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	ready := true
	cfg := fixtureConfigFor(t, f, limits.Default())
	cfg.CredentialsReady = func(context.Context) (bool, error) { return ready, nil }
	svc, _, _ := appFromConfig(t, cfg)

	first := submitStatus(t, svc, "recover-1")

	// Credentials disappear after acceptance (logout, keyring loss). The
	// retry must still reconcile the original submission.
	ready = false
	out, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "recover-1",
		ClientContext:   &contract.ClientContext{ThreadID: "thread-1"},
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	})
	if appErr != nil {
		t.Fatalf("replay without credentials: %s", appErr.Message)
	}
	if out.SubmissionID != first {
		t.Fatalf("replay returned %s, want %s", out.SubmissionID, first)
	}

	// A genuinely new submission still fails closed while unauthorized.
	if _, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "recover-2",
		ClientContext:   &contract.ClientContext{ThreadID: "thread-1"},
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	}); appErr == nil || appErr.Code != contract.CodeAuthenticationRequired {
		t.Fatalf("new submission = %+v, want authentication_required", appErr)
	}
}

// TestSubmitNormalizesClientContextIdentity pins the identity
// normalization: a context that carries no mappable correlation value is
// normalized to absence, so omitting the context and sending an empty one
// are one identity, and an irrelevant thread ID never conflicts when the
// operation has no thread binding. A genuinely different client-visible
// request still conflicts.
func TestSubmitNormalizesClientContextIdentity(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	cfg := fixtureConfigFor(t, f, limits.Default())
	// Drop the thread binding so the context carries no mappable value,
	// and keep the pinned digest honest.
	for i := range cfg.Profile.Operations {
		if cfg.Profile.Operations[i].Name == "status" {
			cfg.Profile.Operations[i].Bindings = nil
			if digest, err := cfg.Profile.Operations[i].ComputeDigest(); err == nil {
				cfg.Profile.Operations[i].Digest = digest
			}
		}
	}
	svc, _, _ := appFromConfig(t, cfg)

	// An absent context is accepted...
	first, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "norm-1",
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	})
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	// ...and a retry with an empty context is the same identity.
	out, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "norm-1",
		ClientContext:   &contract.ClientContext{},
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	})
	if appErr != nil {
		t.Fatalf("empty-context retry: %s", appErr.Message)
	}
	if out.SubmissionID != first.SubmissionID {
		t.Fatalf("empty-context retry returned %s, want %s", out.SubmissionID, first.SubmissionID)
	}
	// An irrelevant thread ID is not part of the identity either.
	out, appErr = svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "norm-1",
		ClientContext:   &contract.ClientContext{ThreadID: "t-irrelevant"},
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	})
	if appErr != nil {
		t.Fatalf("irrelevant-thread retry: %s", appErr.Message)
	}
	if out.SubmissionID != first.SubmissionID {
		t.Fatalf("irrelevant-thread retry returned %s, want %s", out.SubmissionID, first.SubmissionID)
	}
	// A genuinely different client-visible request still conflicts.
	if _, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "norm-1",
		Arguments:       json.RawMessage(`{"detail":"other"}`),
	}); appErr == nil || appErr.Code != contract.CodeIdempotencyConflict {
		t.Fatalf("different arguments = %+v, want idempotency_conflict", appErr)
	}
}

// TestSubmitReplaysWhenToolReconciledAway pins recovery of a lost submit
// response when profile reconciliation removed its tool after acceptance:
// the retry reconciles the durable submission before any current catalog
// state applies, and a genuinely new request for the removed tool still
// fails closed.
func TestSubmitReplaysWhenToolReconciledAway(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	cfg := fixtureConfigFor(t, f, limits.Default())
	svc, _, _ := appFromConfig(t, cfg)

	first := submitStatus(t, svc, "recon-1")

	// Profile reconciliation removes the tool after acceptance.
	cfg.Profile.Operations = nil

	out, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "recon-1",
		ClientContext:   &contract.ClientContext{ThreadID: "thread-1"},
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	})
	if appErr != nil {
		t.Fatalf("replay of a reconciled-away tool: %s", appErr.Message)
	}
	if out.SubmissionID != first {
		t.Fatalf("replay returned %s, want %s", out.SubmissionID, first)
	}

	// A genuinely new request for the removed tool still fails closed.
	if _, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "recon-2",
		ClientContext:   &contract.ClientContext{ThreadID: "thread-1"},
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	}); appErr == nil || appErr.Code != contract.CodeOperationNotAllowed {
		t.Fatalf("new submission = %+v, want operation_not_allowed", appErr)
	}
}

// TestSubmitReplaysWhenBindingsChange pins that retry recovery does not
// depend on the binding set in force now: the stored identity took the
// shape the operation could map at acceptance, and reconciliation may
// have changed that mapping since. Removing a thread binding must not
// turn an exact retry into a conflict, and adding one must not either.
func TestSubmitReplaysWhenBindingsChange(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	cfg := fixtureConfigFor(t, f, limits.Default())
	svc, _, _ := appFromConfig(t, cfg)

	// setStatusThreadBinding reconciles the status tool with or without
	// its thread-ID binding, keeping the pinned digest honest.
	setStatusThreadBinding := func(with bool) {
		for i := range cfg.Profile.Operations {
			if cfg.Profile.Operations[i].Name != "status" {
				continue
			}
			bindings := []catalog.Binding{
				{Source: catalog.SourceClientRequestID, Target: "/client_meta/client_request_id", Required: true},
			}
			if with {
				bindings = append(bindings, catalog.Binding{
					Source: catalog.SourceClientContextThreadID, Target: "/client_meta/thread_id", Required: false,
				})
			}
			cfg.Profile.Operations[i].Bindings = bindings
			if digest, err := cfg.Profile.Operations[i].ComputeDigest(); err == nil {
				cfg.Profile.Operations[i].Digest = digest
			}
		}
	}

	retry := func(clientRequestID, threadID string) (string, *contract.Error) {
		out, appErr := svc.Submit(context.Background(), contract.SubmitInput{
			Tool:            "status",
			ClientRequestID: clientRequestID,
			ClientContext:   &contract.ClientContext{ThreadID: threadID},
			Arguments:       json.RawMessage(`{"detail":"unit"}`),
		})
		if appErr != nil {
			return "", appErr
		}
		return out.SubmissionID, nil
	}

	// (a) Accepted with the thread binding: the stored identity carries
	// the thread ID. Reconciliation removes the binding; the exact retry
	// must still reconcile.
	first, _ := retry("bind-a", "thread-1")
	setStatusThreadBinding(false)
	got, appErr := retry("bind-a", "thread-1")
	if appErr != nil {
		t.Fatalf("retry after the thread binding was removed: %s", appErr.Message)
	}
	if got != first {
		t.Fatalf("retry after binding removal returned %s, want %s", got, first)
	}

	// (b) Accepted without the thread binding: the stored identity does
	// not carry it. Reconciliation adds the binding; the exact retry —
	// whose thread value is only now mappable — must still reconcile.
	second, _ := retry("bind-b", "thread-2")
	setStatusThreadBinding(true)
	got, appErr = retry("bind-b", "thread-2")
	if appErr != nil {
		t.Fatalf("retry after the thread binding was added: %s", appErr.Message)
	}
	if got != second {
		t.Fatalf("retry after binding addition returned %s, want %s", got, second)
	}

	// A retry that actually changed the correlation value still conflicts
	// when the accepted identity carried it.
	if _, appErr := retry("bind-a", "thread-3"); appErr == nil || appErr.Code != contract.CodeIdempotencyConflict {
		t.Fatalf("changed thread ID = %+v, want idempotency_conflict", appErr)
	}
}

// TestSubmitRejectsRetryThatAddsMappedThread pins the explicit-absence
// shape: when the accepted operation could map the client thread ID but
// the accepted request carried no value, the durable identity records the
// source as mappable-but-absent. Reusing the key with a value added would
// change the bound upstream request, so it conflicts — even after
// reconciliation removed the binding, because the stored identity
// remembers the source was mappable at acceptance.
func TestSubmitRejectsRetryThatAddsMappedThread(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	cfg := fixtureConfigFor(t, f, limits.Default())
	svc, _, _ := appFromConfig(t, cfg)

	// The status tool maps the thread ID; the accepted request omits it.
	out, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "absent-1",
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	})
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}

	// An unchanged retry still reconciles.
	retried, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "absent-1",
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	})
	if appErr != nil {
		t.Fatalf("unchanged retry: %s", appErr.Message)
	}
	if retried.SubmissionID != out.SubmissionID {
		t.Fatalf("retry returned %s, want %s", retried.SubmissionID, out.SubmissionID)
	}

	// Adding a thread value under the same key conflicts.
	if _, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "absent-1",
		ClientContext:   &contract.ClientContext{ThreadID: "t-added"},
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	}); appErr == nil || appErr.Code != contract.CodeIdempotencyConflict {
		t.Fatalf("added thread = %+v, want idempotency_conflict", appErr)
	}

	// ...and still conflicts after reconciliation removes the binding:
	// the stored identity recorded the source as mappable at acceptance.
	for i := range cfg.Profile.Operations {
		if cfg.Profile.Operations[i].Name != "status" {
			continue
		}
		cfg.Profile.Operations[i].Bindings = []catalog.Binding{
			{Source: catalog.SourceClientRequestID, Target: "/client_meta/client_request_id", Required: true},
		}
		if digest, err := cfg.Profile.Operations[i].ComputeDigest(); err == nil {
			cfg.Profile.Operations[i].Digest = digest
		}
	}
	if _, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "absent-1",
		ClientContext:   &contract.ClientContext{ThreadID: "t-added"},
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	}); appErr == nil || appErr.Code != contract.CodeIdempotencyConflict {
		t.Fatalf("added thread after binding removal = %+v, want idempotency_conflict", appErr)
	}
}

// TestSubmitProbesCredentialsBeforeAccepting pins the authenticate-first
// contract: a profile with no usable credential fails submit as
// authentication_required before durable acceptance, and the idempotency
// key stays free, so the reauthorize-and-retry flow works.
func TestSubmitProbesCredentialsBeforeAccepting(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	ready := false
	cfg := fixtureConfigFor(t, f, limits.Default())
	cfg.CredentialsReady = func(context.Context) (bool, error) { return ready, nil }
	svc, st, _ := appFromConfig(t, cfg)

	out, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "cred-1",
		Arguments:       json.RawMessage(`{}`),
	})
	if appErr == nil || appErr.Code != contract.CodeAuthenticationRequired {
		t.Fatalf("Submit = %+v err=%v, want authentication_required", out, appErr)
	}
	// Nothing was durably accepted: the key must still be free.
	if _, err := st.GetSubmission(context.Background(), "cred-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSubmission = %v, want not found (key not consumed)", err)
	}

	// After reauthorization the same key is accepted.
	ready = true
	out, appErr = svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: "cred-1",
		Arguments:       json.RawMessage(`{}`),
	})
	if appErr != nil {
		t.Fatalf("retry after authorization: %s", appErr.Message)
	}
	if out.SubmissionID == "" {
		t.Fatalf("no submission id: %+v", out)
	}
}
