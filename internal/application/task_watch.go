package application

import (
	"context"
	"errors"
	"time"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/upstream"
)

func (r *taskRunner) watch(
	ctx context.Context,
	cn *tama2026.Connection,
	submissionID, taskID string,
	apply func(*tama2026.TaskSnapshot) (bool, error),
	reconcile func() (bool, error),
	terminal chan<- struct{},
) {
	delay := taskPollFloor
	for {
		if ctx.Err() != nil {
			return
		}
		if !r.credentialCurrent() {
			if err := r.refreshCredential(ctx); err != nil {
				return
			}
		}
		streamCtx, stop := r.boundStream(ctx)
		received := false
		finished := false
		err := cn.WatchTask(streamCtx, taskID, func(snap tama2026.TaskSnapshot) error {
			received = true
			done, applyErr := apply(&snap)
			if applyErr != nil {
				return applyErr
			}
			if done {
				finished = true
				signal(terminal)
				return context.Canceled
			}
			return nil
		})
		expired := streamCtx.Err() != nil && ctx.Err() == nil
		stop()
		if ctx.Err() != nil || finished {
			return
		}
		if errors.Is(err, tama2026.ErrSubscriptionUnavailable) || errors.Is(err, tama2026.ErrTaskNotSubscribed) {
			return
		}
		if expired || upstream.IsAuth(err) || errors.Is(err, tama2026.ErrAuthenticationRequired) {
			if err := r.refreshCredential(ctx); err != nil {
				return
			}
		}
		if reconcile != nil {
			done, recErr := reconcile()
			if recErr != nil || done {
				return
			}
		}
		delay = nextWatchDelay(delay, received, r.subscriptionBackoffCap(ctx, submissionID))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// nextWatchDelay backs off after a stream that delivered no snapshot and
// resets after one that did. The cap is the stored poll interval so a
// dropped subscription does not outrun tasks/get.
func nextWatchDelay(current time.Duration, received bool, capDelay time.Duration) time.Duration {
	if received {
		return taskPollFloor
	}
	if capDelay < taskPollFloor {
		capDelay = taskPollFloor
	}
	next := current * 2
	if next < current || next > capDelay {
		return capDelay
	}
	return next
}

func (r *taskRunner) subscriptionBackoffCap(ctx context.Context, id string) time.Duration {
	sub, err := r.store.GetSubmission(ctx, id)
	if err != nil || sub.TaskPollIntervalMs <= 0 {
		return taskPollFloor
	}
	return timerWait(sub.TaskPollIntervalMs)
}

func (r *taskRunner) credentialCurrent() bool {
	if r.credentials == nil {
		return true
	}
	expiry, ok := r.credentials.Expiry()
	return ok && expiry.After(r.now())
}

func (r *taskRunner) refreshCredential(ctx context.Context) error {
	if r.credentials == nil {
		return nil
	}
	_, err := r.credentials.Refresh(ctx)
	return err
}

// boundStream returns a context that ends no later than the access-token
// expiry. The parent context remains the cancellation path for shutdown.
func (r *taskRunner) boundStream(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if r.credentials == nil {
		return ctx, cancel
	}
	expiry, ok := r.credentials.Expiry()
	if !ok || !expiry.After(r.now()) {
		cancel()
		return ctx, cancel
	}
	remaining := expiry.Sub(r.now())
	if r.after != nil {
		ch := r.after(remaining)
		go func() {
			select {
			case <-parent.Done():
				cancel()
			case <-ctx.Done():
			case <-ch:
				cancel()
			}
		}()
		return ctx, cancel
	}
	timer := time.AfterFunc(remaining, cancel)
	return ctx, func() {
		timer.Stop()
		cancel()
	}
}

func signal(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
