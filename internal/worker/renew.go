package worker

import (
	"context"
	"fmt"
	"time"
)

func (r *Runner) renew(ctx context.Context, cancel context.CancelFunc, leaseName, leaseOwner string, done chan<- error) {
	interval := r.ttl / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			owned, err := r.state.RenewLease(ctx, leaseName, leaseOwner, r.ttl)
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
		}
	}
}
