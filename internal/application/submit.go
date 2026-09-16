package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
)

// Submit validates one submit call against the pinned profile catalog,
// applies the reviewed bindings, durably accepts the operation, and starts
// its execution path. Success means Tama Link durably accepted
// responsibility; it does not mean Tama accepted or completed the work.
func (s *Service) Submit(ctx context.Context, in contract.SubmitInput) (contract.SubmitOutput, *contract.Error) {
	d, ok := s.profile.Catalog().Find(in.Tool)
	if !ok {
		return contract.SubmitOutput{}, failed(contract.CodeOperationNotAllowed,
			"Operation %q is not approved by the selected profile.", in.Tool)
	}
	clientRequestID := in.ClientRequestID
	if clientRequestID == "" {
		// The public contract makes client_request_id optional: when the
		// client omits it, Tama Link generates one before the first
		// upstream mutation. A generated key is fresh on every call, so
		// each omission is a new submission rather than an idempotent
		// retry.
		var b [12]byte
		if _, err := rand.Read(b[:]); err != nil {
			return contract.SubmitOutput{}, failed(contract.CodeInternal, "%v", err)
		}
		clientRequestID = "crid_" + hex.EncodeToString(b[:])
		in.ClientRequestID = clientRequestID
	}

	// Reconcile an existing idempotency record from the client-visible
	// request BEFORE any current-profile state applies: schema validation,
	// bindings, credential readiness, and the strategy gate are acceptance
	// checks for genuinely new work and must not block recovery of an
	// already accepted request, even when the profile was reconciled or
	// credentials were removed since acceptance. The lookup is read-only,
	// so a miss leaves the key free for a genuinely new acceptance below.
	// A generated key is fresh on every call and skips the lookup.
	identity, err := requestIdentity(in)
	if err != nil {
		return contract.SubmitOutput{}, failed(contract.CodeInvalidRequest,
			"Arguments must be a valid JSON document.")
	}
	if clientRequestID != "" {
		sub, found, err := s.store.ReconcileIdempotentSubmission(ctx, clientRequestID, d.Name, identity)
		if err != nil {
			if errors.Is(err, store.ErrIdempotencyConflict) {
				return contract.SubmitOutput{}, failed(contract.CodeIdempotencyConflict,
					"client_request_id was already used with different arguments for this profile.")
			}
			return contract.SubmitOutput{}, s.storeError(err)
		}
		if found {
			return submitOutput(sub), nil
		}
	}

	clientSchema := d.ClientSchema
	if len(clientSchema) == 0 {
		clientSchema = d.InputSchema
	}
	if err := catalog.ValidateAgainstSchema(clientSchema, in.Arguments); err != nil {
		return contract.SubmitOutput{}, failed(contract.CodeInvalidRequest,
			"Arguments do not satisfy the pinned client schema: %v", err)
	}
	upstreamArgs, err := applyBindings(d, in)
	if err != nil {
		return contract.SubmitOutput{}, failed(contract.CodeInvalidRequest, "%v", err)
	}
	if err := catalog.ValidateAgainstSchema(d.InputSchema, upstreamArgs); err != nil {
		return contract.SubmitOutput{}, failed(contract.CodeInvalidRequest,
			"Arguments do not satisfy the pinned upstream schema: %v", err)
	}

	// Authenticate before durable acceptance: a profile with no usable
	// credential would turn every accepted submission into a terminal
	// authentication_required failure, and the key would then forever
	// replay that failure instead of the reauthorized retry.
	if s.credentialsReady != nil {
		ready, err := s.credentialsReady(ctx)
		if err != nil {
			return contract.SubmitOutput{}, failed(contract.CodeStateUnavailable,
				"Cannot verify profile credentials: %v", err)
		}
		if !ready {
			return contract.SubmitOutput{}, failed(contract.CodeAuthenticationRequired,
				"Tama requires authentication. Complete authorization for the selected profile, then retry.")
		}
	}
	// The strategy gate runs only for genuinely new acceptance: rejected
	// calls leave no row behind, so the same client_request_id stays free
	// for a clean retry.
	if be := s.strategyGate(d); be != nil {
		return contract.SubmitOutput{}, be
	}

	id, err := newSubmissionID()
	if err != nil {
		return contract.SubmitOutput{}, failed(contract.CodeInternal, "%v", err)
	}
	sub, created, err := s.store.CreateSubmission(ctx, store.NewSubmission{
		ID:               id,
		ClientRequestID:  clientRequestID,
		Tool:             d.Name,
		Strategy:         string(d.Strategy),
		DescriptorDigest: d.Digest,
		Arguments:        upstreamArgs,
		RequestArguments: identity,
		ProtocolVersion:  tama2026.ProtocolVersion(),
		AdapterVersion:   s.adapterVersion,
	})
	if err != nil {
		if errors.Is(err, store.ErrIdempotencyConflict) {
			return contract.SubmitOutput{}, failed(contract.CodeIdempotencyConflict,
				"client_request_id was already used with different arguments for this profile.")
		}
		return contract.SubmitOutput{}, s.storeError(err)
	}
	if created {
		// The submission is durable; a lost progress event is not fatal
		// because the await path re-derives progress from the stored state.
		_ = s.appendAcceptedEvent(ctx, sub)
		// The strategy gate above leaves only local_replayable reachable,
		// so every durably accepted submission is dispatched to the leased
		// worker. A replay of an existing row keeps its original event
		// stream and dispatch: appending an accepted event after the row
		// advanced would corrupt the progress sequence, and re-offering an
		// already running work is at best noise.
		s.worker.Dispatch(sub.ID)
	}
	return submitOutput(sub), nil
}

// submitOutput renders the durable submission for the submit response.
func submitOutput(sub *store.Submission) contract.SubmitOutput {
	return contract.SubmitOutput{
		SubmissionID:    sub.ID,
		Status:          sub.Status,
		ClientRequestID: sub.ClientRequestID,
		SubmittedAt:     &sub.CreatedAt,
		NextPollMS:      1000,
	}
}

// requestIdentity renders the client-visible request the idempotency
// identity covers: the raw argument bytes plus the client-owned correlation
// values that reviewed bindings may map. Current schema, binding, and
// descriptor state is deliberately excluded — those are acceptance checks
// for new work, applied only after reconciliation.
func requestIdentity(in contract.SubmitInput) (json.RawMessage, error) {
	return json.Marshal(struct {
		Arguments     json.RawMessage         `json:"arguments"`
		ClientContext *contract.ClientContext `json:"client_context"`
	}{Arguments: in.Arguments, ClientContext: in.ClientContext})
}

// strategyGate rejects execution strategies the initial production profiles
// do not enable, before any durable acceptance, idempotency claim, or
// upstream mutation. It runs only for genuinely new acceptance: rejected
// calls leave no row behind, so the same client_request_id stays free for a
// clean retry.
func (s *Service) strategyGate(d catalog.Descriptor) *contract.Error {
	switch d.Strategy {
	case catalog.StrategyUpstreamTask:
		return failed(contract.CodeNotImplemented,
			"Task-backed operations are not enabled in this build.")
	case catalog.StrategyLocalGuarded:
		return failed(contract.CodeOperationNotAllowed,
			"Operation %q requires a separately reviewed reconciliation contract and is not enabled.", d.Name)
	case catalog.StrategyUnsupported:
		return failed(contract.CodeOperationNotAllowed,
			"Operation %q is not supported by the selected profile.", d.Name)
	default:
		return nil
	}
}

// appendAcceptedEvent records the acceptance progress event.
func (s *Service) appendAcceptedEvent(ctx context.Context, sub *store.Submission) error {
	event := contract.Event{
		SubmissionID: sub.ID,
		Sequence:     sub.Sequence + 1,
		Timestamp:    s.now().UTC(),
		State:        contract.StatusAccepted,
	}
	_, err := s.store.AppendEvents(ctx, sub.ID, []contract.Event{event})
	return err
}

// storeError maps a durable-state failure to the stable taxonomy.
func (s *Service) storeError(err error) *contract.Error {
	switch {
	case errors.Is(err, store.ErrStateUnavailable):
		return failed(contract.CodeStateUnavailable,
			"The profile state is unavailable. The secure backend must be restored before work can continue.")
	default:
		return failed(contract.CodeInternal, "The local state could not be updated.")
	}
}
