package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	if be := s.strategyGate(d); be != nil {
		return contract.SubmitOutput{}, be
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

	id, err := newSubmissionID()
	if err != nil {
		return contract.SubmitOutput{}, failed(contract.CodeInternal, "%v", err)
	}
	sub, err := s.store.CreateSubmission(ctx, store.NewSubmission{
		ID:               id,
		ClientRequestID:  clientRequestID,
		Tool:             d.Name,
		Strategy:         string(d.Strategy),
		DescriptorDigest: d.Digest,
		Arguments:        upstreamArgs,
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
	// The submission is durable; a lost progress event is not fatal because
	// the await path re-derives progress from the stored state.
	_ = s.appendAcceptedEvent(ctx, sub)
	// The strategy gate above leaves only local_replayable reachable, so
	// every durably accepted submission is dispatched to the leased worker.
	s.worker.Dispatch(sub.ID)
	return contract.SubmitOutput{
		SubmissionID:    sub.ID,
		Status:          sub.Status,
		ClientRequestID: sub.ClientRequestID,
		SubmittedAt:     &sub.CreatedAt,
		NextPollMS:      1000,
	}, nil
}

// strategyGate rejects execution strategies the initial production profiles
// do not enable, before any durable acceptance, idempotency claim, or
// upstream mutation. Rejected calls leave no row behind: the same
// client_request_id stays free for a clean retry.
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
