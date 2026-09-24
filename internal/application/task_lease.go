package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/store"
)

func newTaskRunner(st *store.Store, connect func(context.Context) (*tama2026.Connection, error), cfg TaskConfig) (*taskRunner, error) {
	if st == nil || connect == nil {
		return nil, errors.New("task store and connection resolver are required")
	}
	if cfg.Owner == "" {
		return nil, errors.New("task runner owner is required")
	}
	if cfg.LeaseTTL < time.Millisecond {
		return nil, fmt.Errorf("task lease TTL must be at least %s", time.Millisecond)
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &taskRunner{
		store: st, connect: connect, owner: cfg.Owner, ttl: cfg.LeaseTTL,
		now: now, credentials: cfg.Credentials, after: cfg.After,
	}, nil
}

type taskRunner struct {
	store       *store.Store
	connect     func(context.Context) (*tama2026.Connection, error)
	owner       string
	ttl         time.Duration
	now         func() time.Time
	credentials SessionCredentials
	after       func(time.Duration) <-chan time.Time
}

func (r *taskRunner) renew(ctx context.Context, cancel context.CancelFunc, leaseName, leaseOwner string, done chan<- error) {
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
			renewCtx, cancelRenew := context.WithTimeout(ctx, interval)
			owned, err := r.store.RenewLease(renewCtx, leaseName, leaseOwner, r.ttl)
			cancelRenew()
			if ctx.Err() != nil {
				done <- nil
				return
			}
			if err != nil || !owned {
				cancel()
				if err == nil {
					err = errors.New("lease lost")
				}
				done <- fmt.Errorf("renew task lease: %w", err)
				return
			}
			timer.Reset(interval)
		}
	}
}

func (r *taskRunner) release(leaseName, leaseOwner string) {
	ctx, cancel := context.WithTimeout(context.Background(), r.ttl)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		if err := r.store.ReleaseLease(ctx, leaseName, leaseOwner); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func taskLeaseOwner(base string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return base + "/" + hex.EncodeToString(nonce[:]), nil
}
