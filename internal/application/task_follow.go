package application

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/submission"
	"github.com/kritama/tama-link/internal/upstream"
)

func (r *taskRunner) follow(
	ctx context.Context,
	cn *tama2026.Connection,
	sub *store.Submission,
	leaseName, leaseOwner string,
) error {
	watchCtx, cancelWatch := context.WithCancel(ctx)
	terminal := make(chan struct{}, 1)
	var watchers sync.WaitGroup
	watchers.Add(1)
	defer func() {
		cancelWatch()
		watchers.Wait()
	}()
	var mu sync.Mutex
	apply := func(snap *tama2026.TaskSnapshot) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		current, err := r.store.GetSubmission(ctx, sub.ID)
		if err != nil {
			return false, err
		}
		if submission.Terminal(current.Status) {
			return true, nil
		}
		return r.apply(ctx, current, leaseName, leaseOwner, snap)
	}
	reconcile := func() (bool, error) {
		current, err := r.store.GetSubmission(ctx, sub.ID)
		if err != nil {
			return false, err
		}
		if submission.Terminal(current.Status) {
			signal(terminal)
			return true, nil
		}
		snap, err := cn.GetTask(ctx, current.TaskID, int64(current.AcceptedLimits.ResponseBytes))
		if err != nil {
			return false, err
		}
		done, err := apply(snap)
		if done {
			signal(terminal)
		}
		return done, err
	}
	go func() {
		defer watchers.Done()
		r.watch(watchCtx, cn, sub.ID, sub.TaskID, apply, reconcile, terminal)
	}()

	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		current, err := r.store.GetSubmission(ctx, sub.ID)
		if err != nil {
			return err
		}
		if submission.Terminal(current.Status) {
			return nil
		}
		snap, err := cn.GetTask(ctx, current.TaskID, int64(current.AcceptedLimits.ResponseBytes))
		if err != nil {
			if deferredIfContended(err) != nil {
				if err := r.sleep(ctx, timerWait(current.TaskPollIntervalMs)); err != nil {
					return err
				}
				continue
			}
			return r.failOrDefer(ctx, sub.ID, leaseName, leaseOwner, err)
		}
		done, err := apply(snap)
		if err != nil || done {
			return err
		}
		wait := timerWait(snap.PollIntervalMs)
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-terminal:
			return nil
		case <-timer.C:
		}
	}
}

func (r *taskRunner) sleep(ctx context.Context, d time.Duration) error {
	if d < taskPollFloor {
		d = taskPollFloor
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *taskRunner) failOrDefer(ctx context.Context, id, leaseName, leaseOwner string, err error) error {
	if err == nil {
		return nil
	}
	if deferredIfContended(err) != nil || errors.Is(err, store.ErrBusy) || errors.Is(err, store.ErrLeaseNotOwned) {
		return nil
	}
	// A lost tasks/get is not a terminal outcome. Authorization, protocol,
	// and indistinguishable missing/unauthorized lookups are.
	if responseTooLarge(err) || errors.Is(err, tama2026.ErrTaskUnavailable) ||
		errors.Is(err, tama2026.ErrAuthenticationRequired) || errors.Is(err, tama2026.ErrProtocolMismatch) ||
		upstream.IsAuth(err) || upstream.IsProtocol(err) {
		return r.fail(ctx, id, leaseName, leaseOwner, classify(err))
	}
	return nil
}

func (r *taskRunner) fail(ctx context.Context, id, leaseName, leaseOwner string, failure *contract.Error) error {
	if failure == nil {
		e := contract.NewError(contract.CodeUpstreamExecutionFailed, "The upstream task could not be completed.")
		failure = &e
	}
	// A tasks/get failure must not terminalize between the outstanding-ID
	// check and tasks/update. Defer while that delivery lease is held; the
	// sweep retries once the update has been sent or abandoned.
	release, held, err := claimInputDeliveryOnce(ctx, r.store, id)
	if err != nil {
		if errors.Is(err, store.ErrBusy) {
			return nil
		}
		return err
	}
	if !held {
		return nil
	}
	defer release()
	state := contract.StatusFailed
	if failure.Code == contract.CodeOutcomeUnknown {
		state = contract.StatusOutcomeUnknown
	}
	sub, err := r.store.GetSubmission(ctx, id)
	if err != nil {
		return err
	}
	if submission.Terminal(sub.Status) {
		return nil
	}
	if sub.Status == contract.StatusAccepted || sub.Status == contract.StatusQueued {
		if _, err := r.store.TransitionLeased(ctx, id, leaseName, leaseOwner, contract.StatusRunning, store.TransitionDetail{}); err != nil {
			return err
		}
	}
	_, err = r.store.TransitionLeased(ctx, id, leaseName, leaseOwner, state, store.TransitionDetail{Error: failure})
	if err != nil {
		return err
	}
	r.note(ctx, sub, state, "")
	return nil
}
