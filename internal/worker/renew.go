package worker

import (
	"context"
	"fmt"
	"time"
)

func (r *Runner) renew(ctx context.Context, cancel context.CancelFunc, leaseName, leaseOwner string, done chan<- error) {
	interval := r.renewalInterval()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-timer.C:
			owned, err := r.renewOnce(ctx, leaseName, leaseOwner)
			if ctx.Err() != nil {
				done <- nil
				return
			}
			if err != nil {
				cancel()
				done <- fmt.Errorf("%w: %w", ErrLeaseLost, err)
				return
			}
			if !owned {
				cancel()
				done <- ErrLeaseLost
				return
			}
			timer.Reset(interval)
		}
	}
}

func (r *Runner) renewalInterval() time.Duration {
	interval := r.ttl / 3
	if interval <= 0 {
		return r.ttl
	}
	return interval
}

func (r *Runner) renewOnce(ctx context.Context, leaseName, leaseOwner string) (bool, error) {
	renewCtx, cancel := context.WithTimeout(ctx, r.renewalInterval())
	defer cancel()
	return r.state.RenewLease(renewCtx, leaseName, leaseOwner, r.ttl)
}
