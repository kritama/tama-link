package worker

import (
	"context"
	"fmt"
	"time"
)

func (r *Runner) renew(ctx context.Context, cancel context.CancelFunc, leaseName, leaseOwner string, done chan<- error) {
	interval := r.ttl / 3
	if interval <= 0 {
		interval = r.ttl
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-timer.C:
			renewCtx, stopRenew := context.WithTimeout(ctx, interval)
			owned, err := r.state.RenewLease(renewCtx, leaseName, leaseOwner, r.ttl)
			stopRenew()
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
