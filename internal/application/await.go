package application

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/submission"
)

// awaitPollInterval bounds one local re-read while an await long-poll waits
// on a locally executed operation.
const awaitPollInterval = 250 * time.Millisecond

// Await long-polls one known submission and returns either its current
// durable state or its terminal result. Cancellation stops the local wait
// promptly; it never changes accepted upstream or local work.
func (s *Service) Await(ctx context.Context, in contract.AwaitInput) (contract.AwaitOutput, *contract.Error) {
	sub, err := s.store.GetSubmission(ctx, in.SubmissionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return contract.AwaitOutput{}, failed(contract.CodeSubmissionNotFound,
				"No submission with that identifier exists for this profile.")
		}
		return contract.AwaitOutput{}, s.storeError(err)
	}
	after, be := parseCursor(in.Cursor)
	if be != nil {
		return contract.AwaitOutput{}, be
	}
	// A cursor beyond this submission's sequence is a client error (for
	// example a cursor copied from another submission). Echoing it would
	// filter out every genuine event until the sequence catches up, so it
	// is rejected instead of creating a persistent event gap.
	if after > sub.Sequence {
		return contract.AwaitOutput{}, failed(contract.CodeInvalidRequest,
			"cursor %s is beyond the submission's event sequence %d.", in.Cursor, sub.Sequence)
	}
	if be := s.beforeWait(ctx, sub, in); be != nil {
		return contract.AwaitOutput{}, be
	}
	wait, be := s.resolveWait(in.TimeoutMS)
	if be != nil {
		return contract.AwaitOutput{}, be
	}
	if wait > 0 && !submission.Terminal(sub.Status) {
		sub, be = s.waitForState(ctx, sub, wait)
		if be != nil {
			return contract.AwaitOutput{}, be
		}
	}
	return s.buildOutput(sub, after), nil
}

// beforeWait performs the work one await call may do before its bounded
// wait: input-response handling for task-backed submissions. System
// submissions never produce input requests, so any responses for them are
// invalid.
func (s *Service) beforeWait(_ context.Context, sub *store.Submission, in contract.AwaitInput) *contract.Error {
	if len(in.InputResponses) == 0 {
		return nil
	}
	// Input requests exist only in the App task workflow. No submission can
	// be input_required in this build slice, so any responses are invalid.
	if sub.Status != contract.StatusInputRequired {
		return failed(contract.CodeInvalidRequest,
			"input_responses is only accepted while the submission is input_required.")
	}
	return failed(contract.CodeNotImplemented,
		"Task-backed input responses are not enabled in this build.")
}

// resolveWait maps timeout_ms to one bounded long-poll duration. Zero uses
// the profile default; values above the maximum are rejected, never clamped
// silently.
func (s *Service) resolveWait(timeoutMS int) (time.Duration, *contract.Error) {
	limit := s.profile.EffectiveLimits()
	if timeoutMS < 0 {
		return 0, failed(contract.CodeInvalidRequest, "timeout_ms must be non-negative.")
	}
	if timeoutMS == 0 {
		return limit.AwaitDefault, nil
	}
	// Compare in milliseconds before any duration conversion: a value
	// beyond the maximum must fail, and converting it first overflows
	// time.Duration on 64-bit builds and can wrap negative.
	if int64(timeoutMS) > int64(limit.AwaitMax)/int64(time.Millisecond) {
		return 0, failed(contract.CodeInvalidRequest,
			"timeout_ms exceeds the profile maximum of %s.", limit.AwaitMax)
	}
	return time.Duration(timeoutMS) * time.Millisecond, nil
}

// waitForState re-reads the submission until it is terminal, the budget is
// spent, or the caller cancels. The returned submission is always a fresh
// read.
func (s *Service) waitForState(ctx context.Context, sub *store.Submission, budget time.Duration) (*store.Submission, *contract.Error) {
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	for !submission.Terminal(sub.Status) {
		select {
		case <-ctx.Done():
			// The caller gave up on waiting; report the freshest state.
			return s.finalState(sub)
		case <-deadline.C:
			return s.finalState(sub)
		case <-time.After(awaitPollInterval):
		}
		reloaded, err := s.store.GetSubmission(ctx, sub.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, failed(contract.CodeSubmissionNotFound,
					"No submission with that identifier exists for this profile.")
			}
			return nil, s.storeError(err)
		}
		sub = reloaded
	}
	return sub, nil
}

// defaultFinalReadTimeout bounds the one final state read after the caller
// gave up or the budget expired. The read must not be cancelled with the
// caller — a fresh terminal state is more useful than the last snapshot —
// but it also must not hold the MCP handler beyond the wait contract while
// SQLite is contended or stalled.
const defaultFinalReadTimeout = 2 * time.Second

// finalReadTimeoutOverride holds a test override in nanoseconds; zero means
// the default. Atomic so parallel tests that read the deadline never race a
// test that overrides it.
var finalReadTimeoutOverride atomic.Int64

// setFinalReadTimeout overrides the final-read deadline; tests use it, with
// zero restoring the default.
func setFinalReadTimeout(d time.Duration) { finalReadTimeoutOverride.Store(int64(d)) }

// finalState performs that final fresh read on an independent context with
// its own short deadline. Only the expiry of that independent deadline
// falls back to the snapshot — at most one poll interval stale, and the
// caller can await again; a real storage failure still reaches the caller
// instead of being reported as a successful pending response.
func (s *Service) finalState(sub *store.Submission) (*store.Submission, *contract.Error) {
	timeout := defaultFinalReadTimeout
	if v := finalReadTimeoutOverride.Load(); v != 0 {
		timeout = time.Duration(v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	reloaded, cerr := s.reload(ctx, sub.ID)
	if cerr != nil {
		if ctx.Err() != nil {
			return sub, nil
		}
		return nil, cerr
	}
	return reloaded, nil
}

func (s *Service) reload(ctx context.Context, id string) (*store.Submission, *contract.Error) {
	reloaded, err := s.store.GetSubmission(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, failed(contract.CodeSubmissionNotFound,
				"No submission with that identifier exists for this profile.")
		}
		return nil, s.storeError(err)
	}
	return reloaded, nil
}

// parseCursor decodes one opaque progress cursor: the decimal sequence of
// the last observed event.
func parseCursor(cursor string) (int64, *contract.Error) {
	if cursor == "" {
		return 0, nil
	}
	if !isDigits(cursor) {
		return 0, failed(contract.CodeInvalidRequest, "cursor is not a valid progress cursor.")
	}
	sequence, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil || sequence < 0 {
		return 0, failed(contract.CodeInvalidRequest, "cursor is not a valid progress cursor.")
	}
	return sequence, nil
}

func isDigits(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// buildOutput renders one await response from a fresh submission read.
func (s *Service) buildOutput(sub *store.Submission, after int64) contract.AwaitOutput {
	events := make([]contract.Event, 0, len(sub.Events))
	for _, event := range sub.Events {
		if event.Sequence > after {
			events = append(events, event)
		}
	}
	last := after
	for _, event := range sub.Events {
		if event.Sequence > last {
			last = event.Sequence
		}
	}
	out := contract.AwaitOutput{
		SubmissionID: sub.ID,
		Tool:         sub.Tool,
		Status:       sub.Status,
		Terminal:     submission.Terminal(sub.Status),
		Cursor:       strconv.FormatInt(last, 10),
		Events:       events,
	}
	if !submission.Terminal(sub.Status) {
		// Polling guidance is only meaningful for work that can still
		// change: the documented terminal response omits it, so a client
		// scheduling retries off this field stops once terminal is true.
		out.NextPollMS = 1000
		return out
	}
	if sub.CompletedAt != nil {
		out.CompletedAt = sub.CompletedAt
	}
	switch sub.Status {
	case contract.StatusCompleted:
		if sub.Result != nil {
			out.Result = sub.Result
			return out
		}
		// The payload fell out of retention before the read.
		out.Error = contractPtr(contract.NewError(contract.CodeSubmissionExpired,
			"The terminal result is no longer retained for this submission."))
		return out
	case contract.StatusExpired:
		out.Error = contractPtr(contract.NewError(contract.CodeSubmissionExpired,
			"The terminal result is no longer retained for this submission."))
		return out
	}
	if sub.ErrorCode != "" {
		err := contract.Error{
			Code:      contract.Code(sub.ErrorCode),
			Message:   sub.ErrorMessage,
			Retryable: sub.ErrorRetryable,
		}
		out.Error = &err
	}
	return out
}

func contractPtr(e contract.Error) *contract.Error { return &e }
