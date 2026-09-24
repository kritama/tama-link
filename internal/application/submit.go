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
	// request BEFORE any current-profile state applies: catalog
	// membership, schema validation, bindings, credential readiness, and
	// the strategy gate are acceptance checks for genuinely new work and
	// must not block recovery of an already accepted request, even when
	// the profile was reconciled away from its tool, its bindings changed,
	// or credentials were removed since acceptance. The stored identity
	// was shaped by the binding set in force at acceptance, which a retry
	// cannot know, so the candidates — the full client-visible identity
	// and, when the context carries a thread ID, the identity without it
	// — cover every accepted shape without consulting the current
	// descriptor. The lookup is read-only, so a miss leaves the key free
	// for a genuinely new acceptance below. A generated key is fresh on
	// every call and skips the lookup.
	identities, err := retryIdentities(in)
	if err != nil {
		return contract.SubmitOutput{}, failed(contract.CodeInvalidRequest,
			"Arguments must be a valid JSON document.")
	}
	if clientRequestID != "" {
		sub, found, err := s.store.ReconcileIdempotentSubmission(ctx, clientRequestID, in.Tool, identities)
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

	d, ok := s.profile.Catalog().Find(in.Tool)
	if !ok {
		return contract.SubmitOutput{}, failed(contract.CodeOperationNotAllowed,
			"Operation %q is not approved by the selected profile.", in.Tool)
	}

	// The accepted identity carries the correlation values the pinned
	// operation can map: when the operation cannot map the thread ID, the
	// value is not part of the request's durable identity at all.
	identity, err := requestIdentity(in, identityThread(in, &d))
	if err != nil {
		return contract.SubmitOutput{}, failed(contract.CodeInvalidRequest,
			"Arguments must be a valid JSON document.")
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
		// When a concurrent process claims the key under a different
		// binding set, the insert loses the race and reconciles against
		// every identity shape this request may correspond to — the same
		// candidates the read-only reconciliation above used.
		IdentityCandidates: identities,
		ProtocolVersion:    tama2026.ProtocolVersion(),
		AdapterVersion:     s.adapterVersion,
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
		if d.Strategy == catalog.StrategyUpstreamTask {
			s.tasks.Dispatch(sub.ID)
		} else {
			s.worker.Dispatch(sub.ID)
		}
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

// requestIdentity renders the client-visible request with one
// correlation shape: the raw argument bytes plus the thread-ID field.
// A nil thread records that the accepted operation could not map the
// source: the field is omitted and any client thread value is not part
// of the identity at all. A non-nil thread records that the source was
// mappable at acceptance: the field carries the string value, or an
// explicit JSON null when the request carried no value — never an empty
// string, so an absent-to-present change stays a conflict even after
// reconciliation unmapped the source.
func requestIdentity(in contract.SubmitInput, threadID *string) (json.RawMessage, error) {
	identity := struct {
		Arguments json.RawMessage `json:"arguments"`
		ThreadID  json.RawMessage `json:"thread_id,omitempty"`
	}{Arguments: in.Arguments}
	if threadID != nil {
		if *threadID == "" {
			identity.ThreadID = json.RawMessage(`null`)
		} else {
			encoded, err := json.Marshal(*threadID)
			if err != nil {
				return nil, err
			}
			identity.ThreadID = encoded
		}
	}
	return json.Marshal(identity)
}

// identityThread is the correlation shape the accepted identity records
// for a genuinely new acceptance: absent when the pinned operation cannot
// map the client thread ID, explicit null when it could but the request
// carried no value, and the value otherwise.
func identityThread(in contract.SubmitInput, d *catalog.Descriptor) *string {
	if !mapsThreadID(d) {
		return nil
	}
	value := ""
	if in.ClientContext != nil {
		value = in.ClientContext.ThreadID
	}
	return &value
}

// retryIdentities renders the candidate client-visible identities an exact
// retry may correspond to. The stored identity recorded how the accepted
// operation handled the client thread ID — value, explicit null, or
// omitted field — and the retry cannot know which shape it took: the
// binding set in force at acceptance may have changed since. A retry that
// carries a value matches the identity with its own value, or the identity
// whose operation could not map the source; a retry that carries no value
// matches the identity with the explicit null, or the identity whose
// operation could not map the source. Adding a thread ID after the
// accepted request omitted one from a mappable source conflicts; changing
// a value the operation never mapped reconciles; an exact retry reconciles
// no matter what reconciliation did to the tool since.
func retryIdentities(in contract.SubmitInput) ([]json.RawMessage, error) {
	base, err := requestIdentity(in, nil)
	if err != nil {
		return nil, err
	}
	value := ""
	if in.ClientContext != nil {
		value = in.ClientContext.ThreadID
	}
	threaded, err := requestIdentity(in, &value)
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{threaded, base}, nil
}

// mapsThreadID reports whether the descriptor has a binding that can map
// the client thread ID upstream.
func mapsThreadID(d *catalog.Descriptor) bool {
	for _, b := range d.Bindings {
		if b.Source == catalog.SourceClientContextThreadID {
			return true
		}
	}
	return false
}

// strategyGate rejects execution strategies the initial production profiles
// do not enable, before any durable acceptance, idempotency claim, or
// upstream mutation. It runs only for genuinely new acceptance: rejected
// calls leave no row behind, so the same client_request_id stays free for a
// clean retry.
func (s *Service) strategyGate(d catalog.Descriptor) *contract.Error {
	switch d.Strategy {
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
