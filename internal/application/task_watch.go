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
	taskID string,
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
		finished := false
		err := cn.WatchTask(streamCtx, taskID, func(snap tama2026.TaskSnapshot) error {
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
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
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
