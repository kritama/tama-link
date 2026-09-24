package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/submission"
	"github.com/kritama/tama-link/internal/upstream"
)

// Run drives one App submission until it is terminal, the lease is lost, or
// ctx is cancelled. Cancellation stops local polling and subscriptions. It
// does not call tasks/cancel.
func (r *taskRunner) Run(ctx context.Context, id string) error {
	leaseName := "submission/" + id
	leaseOwner, err := taskLeaseOwner(r.owner)
	if err != nil {
		return fmt.Errorf("create task lease owner: %w", err)
	}
	owned, err := r.store.ClaimLease(ctx, leaseName, leaseOwner, r.ttl)
	if err != nil || !owned {
		return err
	}
	defer r.release(leaseName, leaseOwner)

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	renewed := make(chan error, 1)
	go r.renew(execCtx, cancel, leaseName, leaseOwner, renewed)

	runErr := r.drive(execCtx, id, leaseName, leaseOwner)
	cancel()
	renewErr := <-renewed
	if renewErr != nil {
		return renewErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return runErr
}

func (r *taskRunner) drive(ctx context.Context, id, leaseName, leaseOwner string) error {
	sub, first, err := r.prepare(ctx, id, leaseName, leaseOwner)
	if err != nil || sub == nil || submission.Terminal(sub.Status) {
		return err
	}
	cn, err := r.connect(ctx)
	if err != nil {
		return r.failOrDefer(ctx, id, leaseName, leaseOwner, err)
	}
	if err := checkDescriptorDigest(cn, sub); err != nil {
		return r.fail(ctx, id, leaseName, leaseOwner, err)
	}
	sub, err = r.ensureTask(ctx, cn, sub, leaseName, leaseOwner, first)
	if err != nil || sub == nil || sub.TaskID == "" || submission.Terminal(sub.Status) {
		return err
	}
	return r.follow(ctx, cn, sub, leaseName, leaseOwner)
}

func (r *taskRunner) prepare(ctx context.Context, id, leaseName, leaseOwner string) (*store.Submission, bool, error) {
	sub, err := r.store.GetSubmission(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if sub.Strategy != string(catalog.StrategyUpstreamTask) || submission.Terminal(sub.Status) {
		return nil, false, nil
	}
	// accepted and queued have not called upstream yet. OpenTask runs only
	// after the row reaches running, so recovering either status is a first
	// call. A running row with no task ID is the ambiguous replay.
	first := sub.Status == contract.StatusAccepted || sub.Status == contract.StatusQueued
	if sub.Status == contract.StatusAccepted {
		sub, err = r.store.TransitionLeased(ctx, id, leaseName, leaseOwner, contract.StatusQueued, store.TransitionDetail{})
		if err != nil {
			return nil, false, err
		}
	}
	if sub.Status == contract.StatusQueued {
		sub, err = r.store.TransitionLeased(ctx, id, leaseName, leaseOwner, contract.StatusRunning, store.TransitionDetail{})
		if err != nil {
			return nil, false, err
		}
		r.note(ctx, sub, contract.StatusRunning, "")
	}
	return sub, first, nil
}

// ensureTask attaches an owner-bound task ID. A submission that is already
// running without one may already have reached Tama, so tools/call is
// replayed only when the pinned bindings prove Message Submission idempotency.
func (r *taskRunner) ensureTask(
	ctx context.Context,
	cn *tama2026.Connection,
	sub *store.Submission,
	leaseName, leaseOwner string,
	first bool,
) (*store.Submission, error) {
	if sub.TaskID != "" {
		return sub, nil
	}
	d, ok := cn.Catalog().Find(sub.Tool)
	if !ok {
		return nil, r.fail(ctx, sub.ID, leaseName, leaseOwner, checkDescriptorDigest(cn, sub))
	}
	if !first && !replayProven(d) {
		return nil, r.fail(ctx, sub.ID, leaseName, leaseOwner, outcomeUnknown())
	}
	handle, err := cn.OpenTask(ctx, sub.Tool, sub.Arguments)
	if err != nil {
		return nil, r.openFailed(ctx, sub.ID, leaseName, leaseOwner, d, err)
	}
	if err := r.store.AttachTaskID(ctx, sub.ID, handle.TaskID); err != nil && !errors.Is(err, store.ErrTaskIDConflict) {
		return nil, err
	}
	if err := r.observe(ctx, sub.ID, leaseName, leaseOwner, handle); err != nil {
		return nil, err
	}
	return r.store.GetSubmission(ctx, sub.ID)
}

func (r *taskRunner) openFailed(
	ctx context.Context,
	id, leaseName, leaseOwner string,
	d catalog.Descriptor,
	err error,
) error {
	if deferredIfContended(err) != nil || errors.Is(err, store.ErrBusy) {
		return nil
	}
	if responseTooLarge(err) || upstream.IsAuth(err) || errors.Is(err, tama2026.ErrAuthenticationRequired) ||
		errors.Is(err, tama2026.ErrOperationNotAllowed) || errors.Is(err, tama2026.ErrCatalogMismatch) ||
		errors.Is(err, tama2026.ErrProtocolMismatch) || upstream.IsProtocol(err) {
		return r.fail(ctx, id, leaseName, leaseOwner, classify(err))
	}
	// A lost response may already have created the task. Replay only when
	// the pinned bindings prove Message Submission idempotency.
	if replayProven(d) {
		return nil
	}
	return r.fail(ctx, id, leaseName, leaseOwner, outcomeUnknown())
}

func replayProven(d catalog.Descriptor) bool {
	for _, b := range d.Bindings {
		if b.Source == catalog.SourceClientRequestID {
			return true
		}
	}
	return false
}

func outcomeUnknown() *contract.Error {
	e := contract.NewError(contract.CodeOutcomeUnknown,
		"The upstream operation may have started, but its task handle was not received and the request cannot be retried safely.")
	return &e
}
