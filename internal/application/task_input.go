package application

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/jsonvalue"
	"github.com/kritama/tama-link/internal/store"
)

const inputDeliveryTTL = 15 * time.Second

// applyInputResponses validates one await's input map, records new canonical
// responses, and sends at most one tasks/update. Delivery is claimed with a
// per-submission lease so concurrent waits cannot both send. Exact replays of
// already delivered responses do not call upstream again. Only still-pending
// outstanding IDs are sent.
func (s *Service) applyInputResponses(ctx context.Context, sub *store.Submission, in contract.AwaitInput) *contract.Error {
	if len(in.InputResponses) == 0 {
		return nil
	}
	if sub.Strategy != string(catalog.StrategyUpstreamTask) {
		return failed(contract.CodeInvalidRequest,
			"input_responses is only accepted while the submission is input_required.")
	}
	canonical, be := canonicalResponses(in.InputResponses)
	if be != nil {
		return be
	}
	owner, release, be := s.claimInputDelivery(ctx, sub.ID)
	if be != nil {
		return be
	}
	defer release()
	return s.deliverInput(ctx, sub.ID, owner, canonical)
}

func (s *Service) deliverInput(ctx context.Context, id, owner string, canonical map[string]json.RawMessage) *contract.Error {
	name := inputDeliveryLease(id)
	renewCtx, cancelRenew := context.WithCancel(context.Background())
	defer cancelRenew()
	lost := make(chan struct{})
	go s.renewInputDelivery(renewCtx, name, owner, lost)

	sub, err := s.store.GetSubmission(ctx, id)
	if err != nil {
		return s.storeError(err)
	}
	outstanding, err := requestIDs(sub.InputRequests)
	if err != nil {
		return failed(contract.CodeInternal, "Stored input requests are unreadable.")
	}
	ids := sortedKeys(canonical)
	pending := make([]string, 0, len(ids))
	for _, requestID := range ids {
		value := canonical[requestID]
		stored, found, err := s.store.GetInputResponse(ctx, id, requestID)
		if err != nil {
			return s.storeError(err)
		}
		if found && string(stored) != string(value) {
			return failed(contract.CodeIdempotencyConflict,
				"input_responses changed a response that was already recorded.")
		}
		live := sub.Status == contract.StatusInputRequired && slices.Contains(outstanding, requestID)
		if !found && !live {
			return failed(contract.CodeInvalidRequest,
				"input_responses includes an identifier that is not outstanding.")
		}
		if !found {
			if err := s.store.SetInputResponse(ctx, id, requestID, value); err != nil {
				if errors.Is(err, store.ErrInputResponseConflict) {
					return failed(contract.CodeIdempotencyConflict,
						"input_responses changed a response that was already recorded.")
				}
				return s.storeError(err)
			}
		}
		if !live {
			continue
		}
		waiting, err := s.store.InputResponsePending(ctx, id, requestID)
		if err != nil {
			return s.storeError(err)
		}
		if waiting {
			pending = append(pending, requestID)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	if sub.TaskID == "" {
		return failed(contract.CodeUpstreamUnavailable, "The upstream task handle is not available yet.")
	}
	pending, be := s.pendingOutstanding(ctx, id, pending)
	if be != nil || len(pending) == 0 {
		return be
	}
	body := make(map[string]json.RawMessage, len(pending))
	for _, requestID := range pending {
		body[requestID] = canonical[requestID]
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return failed(contract.CodeInternal, "Could not encode input responses.")
	}
	generation, owned, err := s.store.LeaseGeneration(ctx, name, owner)
	if err != nil {
		return s.storeError(err)
	}
	if !owned {
		return failed(contract.CodeUpstreamUnavailable, "Input delivery lost its lease before the update was sent. Retry.")
	}
	updateCtx, cancelUpdate := context.WithCancel(ctx)
	defer cancelUpdate()
	go func() {
		select {
		case <-lost:
			cancelUpdate()
		case <-updateCtx.Done():
		}
	}()
	cn, err := s.connect(updateCtx)
	if err != nil {
		return classify(err)
	}
	if err := cn.UpdateTask(updateCtx, sub.TaskID, encoded); err != nil {
		return classify(err)
	}
	return s.recordDeliveredInput(ctx, id, name, owner, generation, pending)
}

// recordDeliveredInput marks a tasks/update that already succeeded. The
// caller's context can be cancelled after that acceptance; the mark must
// still be attempted so the next replay does not send the same input again.
func (s *Service) recordDeliveredInput(ctx context.Context, id, name, owner string, generation int64, pending []string) *contract.Error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalReadTimeout())
	defer cancel()
	still, err := s.store.CommitLease(persistCtx, name, owner, generation)
	if err != nil {
		return s.storeError(err)
	}
	if !still {
		// The update was sent, but this owner no longer holds the delivery
		// lease. Leave the IDs unmarked so a later exact replay can resend.
		return nil
	}
	if err := s.store.MarkInputResponsesSent(persistCtx, id, pending); err != nil {
		return s.storeError(err)
	}
	return nil
}

// pendingOutstanding re-reads the outstanding set immediately before the
// upstream update. The caller holds the delivery lease, so a task snapshot
// cannot replace that set until the update finishes.
func (s *Service) pendingOutstanding(ctx context.Context, id string, pending []string) ([]string, *contract.Error) {
	sub, err := s.store.GetSubmission(ctx, id)
	if err != nil {
		return nil, s.storeError(err)
	}
	if sub.Status != contract.StatusInputRequired {
		return nil, nil
	}
	outstanding, err := requestIDs(sub.InputRequests)
	if err != nil {
		return nil, failed(contract.CodeInternal, "Stored input requests are unreadable.")
	}
	live := make([]string, 0, len(pending))
	for _, requestID := range pending {
		if !slices.Contains(outstanding, requestID) {
			continue
		}
		waiting, err := s.store.InputResponsePending(ctx, id, requestID)
		if err != nil {
			return nil, s.storeError(err)
		}
		if waiting {
			live = append(live, requestID)
		}
	}
	return live, nil
}

func (s *Service) renewInputDelivery(ctx context.Context, name, owner string, lost chan struct{}) {
	if !s.extendDelivery(name, owner) {
		close(lost)
		return
	}
	interval := s.deliveryTTL() / 3
	if interval < 5*time.Millisecond {
		interval = 5 * time.Millisecond
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if !s.extendDelivery(name, owner) {
				close(lost)
				return
			}
			timer.Reset(interval)
		}
	}
}

func (s *Service) extendDelivery(name, owner string) bool {
	owned, err := s.store.RenewLease(context.Background(), name, owner, s.deliveryTTL())
	return err == nil && owned
}

func (s *Service) deliveryTTL() time.Duration {
	if s.inputDeliveryTTL > 0 {
		return s.inputDeliveryTTL
	}
	return inputDeliveryTTL
}

func (s *Service) claimInputDelivery(ctx context.Context, id string) (string, func(), *contract.Error) {
	owner, err := taskLeaseOwner("input")
	if err != nil {
		return "", nil, failed(contract.CodeInternal, "Could not claim input delivery.")
	}
	name := inputDeliveryLease(id)
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		owned, err := s.store.ClaimLease(ctx, name, owner, s.deliveryTTL())
		if err != nil {
			return "", nil, s.storeError(err)
		}
		if owned {
			return owner, func() {
				_ = s.store.ReleaseLease(context.Background(), name, owner)
			}, nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", nil, failed(contract.CodeUpstreamUnavailable, "Input delivery was cancelled before it acquired the lease.")
		case <-deadline.C:
			timer.Stop()
			return "", nil, failed(contract.CodeUpstreamUnavailable, "Another await is delivering input responses. Retry.")
		case <-timer.C:
		}
	}
}

func inputDeliveryLease(id string) string {
	return "input-delivery/" + id
}

// claimInputDeliveryOnce tries the delivery lease without waiting. Task
// snapshot writes use it so they defer while an await is sending tasks/update.
func claimInputDeliveryOnce(ctx context.Context, st *store.Store, id string) (func(), bool, error) {
	owner, err := taskLeaseOwner("snapshot")
	if err != nil {
		return nil, false, err
	}
	name := inputDeliveryLease(id)
	owned, err := st.ClaimLease(ctx, name, owner, inputDeliveryTTL)
	if err != nil || !owned {
		return nil, false, err
	}
	return func() {
		_ = st.ReleaseLease(context.Background(), name, owner)
	}, true, nil
}

func canonicalResponses(raw map[string]json.RawMessage) (map[string]json.RawMessage, *contract.Error) {
	out := make(map[string]json.RawMessage, len(raw))
	for id, value := range raw {
		canonical, err := jsonvalue.Canonical(value)
		if err != nil || len(value) == 0 {
			return nil, failed(contract.CodeInvalidRequest, "input_responses values must be JSON.")
		}
		out[id] = canonical
	}
	return out, nil
}

func sortedKeys(raw map[string]json.RawMessage) []string {
	ids := make([]string, 0, len(raw))
	for id := range raw {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func requestIDs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return sortedKeys(doc), nil
}
