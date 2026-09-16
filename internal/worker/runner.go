// Package worker executes locally replayable submissions under durable SQLite
// leases. A Runner owns every goroutine it starts and stops execution when it
// loses the lease or its caller cancels the context.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
)

var (
	// ErrLeaseHeld reports that another process currently owns the submission.
	ErrLeaseHeld = errors.New("submission lease held by another worker")
	// ErrNotRunnable reports that a listed submission no longer needs local
	// execution: another worker or the sweep already advanced it past the
	// runnable states. It is progress, not a failure.
	ErrNotRunnable = errors.New("submission not runnable")
	// ErrLeaseLost reports that this worker could not renew its ownership.
	ErrLeaseLost = errors.New("submission lease lost")
)

// State is the durable behavior a Runner consumes.
type State interface {
	ClaimLease(context.Context, string, string, time.Duration) (bool, error)
	RenewLease(context.Context, string, string, time.Duration) (bool, error)
	ReleaseLease(context.Context, string, string) error
	GetSubmission(context.Context, string) (*store.Submission, error)
	ListRunnable(context.Context, string) ([]string, error)
	TransitionLeased(context.Context, string, string, string, contract.Status, store.TransitionDetail) (*store.Submission, error)
	CompleteLeased(context.Context, string, string, string, contract.Result) (*store.Submission, error)
	FailLeased(context.Context, string, string, string, contract.Error) (*store.Submission, error)
}

// Executor performs one ordinary upstream tools/call. Implementations must
// honor context cancellation and must be safe to replay for this worker
// path. An error carrying a *contract.Error records that stable error on
// the submission; any other error records upstream_execution_failed.
type Executor interface {
	Execute(context.Context, *store.Submission) (contract.Result, error)
}

// Config defines one worker identity, lease duration, and sweep cadence.
type Config struct {
	Owner    string
	LeaseTTL time.Duration

	// SweepInterval bounds how long an accepted replayable submission can
	// wait to be rediscovered from the durable store when the in-memory
	// queue was saturated. Zero selects the default.
	SweepInterval time.Duration
}

// Runner coordinates durable state, lease ownership, and execution.
type Runner struct {
	state    State
	executor Executor
	owner    string
	ttl      time.Duration
}

// New returns a fully configured local replay worker.
func New(state State, executor Executor, cfg Config) (*Runner, error) {
	if state == nil || executor == nil {
		return nil, errors.New("worker state and executor are required")
	}
	if cfg.Owner == "" {
		return nil, errors.New("worker owner is required")
	}
	if cfg.LeaseTTL < time.Millisecond {
		return nil, fmt.Errorf("worker lease TTL must be at least %s", time.Millisecond)
	}
	return &Runner{state: state, executor: executor, owner: cfg.Owner, ttl: cfg.LeaseTTL}, nil
}

// Run owns and executes one locally replayable submission.
func (r *Runner) Run(ctx context.Context, id string) (runErr error) {
	leaseName := "submission/" + id
	leaseOwner, err := invocationOwner(r.owner)
	if err != nil {
		return fmt.Errorf("create lease owner for submission %s: %w", id, err)
	}
	owned, err := r.state.ClaimLease(ctx, leaseName, leaseOwner, r.ttl)
	if err != nil {
		return fmt.Errorf("claim submission %s: %w", id, err)
	}
	if !owned {
		return ErrLeaseHeld
	}
	defer func() {
		// The release runs on every path: a cancelled or failed run must give
		// its lease back so recovery can re-claim the still-replayable
		// submission. It is best effort with bounded retries — a lease that
		// survives them only lingers until TTL expiry, and a submission whose
		// terminal state is already published can never be shadowed by it.
		releaseCtx, cancelRelease := context.WithTimeout(context.Background(), r.ttl)
		defer cancelRelease()
		_ = r.releaseLease(releaseCtx, leaseName, leaseOwner)
	}()

	execCtx, cancel := context.WithCancel(ctx)
	renewed := make(chan error, 1)
	go r.renew(execCtx, cancel, leaseName, leaseOwner, renewed)

	sub, err := r.prepare(execCtx, id, leaseName, leaseOwner)
	if err == nil {
		var result contract.Result
		var executeErr error
		result, executeErr = r.executor.Execute(execCtx, sub)
		if executeErr == nil {
			_, err = r.state.CompleteLeased(execCtx, id, leaseName, leaseOwner, result)
		} else if execCtx.Err() == nil {
			failure := contract.NewError(contract.CodeUpstreamExecutionFailed, "The local operation could not be completed.")
			var carrier interface{ ContractError() *contract.Error }
			if errors.As(executeErr, &carrier) {
				failure = *carrier.ContractError()
			}
			if _, transitionErr := r.state.FailLeased(execCtx, id, leaseName, leaseOwner, failure); transitionErr != nil {
				err = transitionErr
			} else {
				err = fmt.Errorf("execute submission %s: %w", id, executeErr)
			}
		} else {
			err = executeErr
		}
	}
	cancel()
	renewErr := <-renewed
	if renewErr != nil {
		return renewErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// releaseLease retries the lease release against transient SQLite
// contention and gives up quietly: the caller has already published a
// terminal state, and the lease expires on its own.
func (r *Runner) releaseLease(ctx context.Context, leaseName, leaseOwner string) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = r.state.ReleaseLease(ctx, leaseName, leaseOwner); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(200 * time.Millisecond):
		}
	}
	return err
}

func invocationOwner(base string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return base + "/" + hex.EncodeToString(nonce[:]), nil
}

// Runnable lists every durable submission the local worker may execute: the
// replayable strategy in a non-terminal state, oldest first.
func (r *Runner) Runnable(ctx context.Context) ([]string, error) {
	return r.state.ListRunnable(ctx, string(catalog.StrategyLocalReplayable))
}

// Recover executes all pending locally replayable submissions. Live leases are
// skipped because their owning process remains responsible for them.
func (r *Runner) Recover(ctx context.Context) error {
	ids, err := r.state.ListRunnable(ctx, string(catalog.StrategyLocalReplayable))
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if err := r.Run(ctx, id); err != nil &&
			!errors.Is(err, ErrLeaseHeld) &&
			!errors.Is(err, ErrNotRunnable) &&
			!errors.Is(err, store.ErrBusy) {
			failures = append(failures, fmt.Errorf("recover %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

func (r *Runner) prepare(ctx context.Context, id, leaseName, leaseOwner string) (*store.Submission, error) {
	sub, err := r.state.GetSubmission(ctx, id)
	if err != nil {
		return nil, err
	}
	if sub.Strategy != string(catalog.StrategyLocalReplayable) {
		return nil, fmt.Errorf("submission %s uses non-replayable strategy %q", id, sub.Strategy)
	}
	switch sub.Status {
	case contract.StatusAccepted:
		_, err = r.state.TransitionLeased(ctx, id, leaseName, leaseOwner, contract.StatusQueued, store.TransitionDetail{})
		if err != nil {
			return nil, preparationError(err)
		}
		fallthrough
	case contract.StatusQueued:
		sub, err = r.state.TransitionLeased(ctx, id, leaseName, leaseOwner, contract.StatusRunning, store.TransitionDetail{})
		if err != nil {
			return nil, preparationError(err)
		}
	case contract.StatusRunning:
		// A recovered replayable operation is deliberately executed again.
	default:
		return nil, fmt.Errorf("%w: submission %s is in state %s", ErrNotRunnable, id, sub.Status)
	}
	owned, err := r.renewOnce(ctx, leaseName, leaseOwner)
	if err != nil {
		return nil, fmt.Errorf("%w: confirm submission %s lease: %w", ErrLeaseLost, id, err)
	}
	if !owned {
		return nil, ErrLeaseLost
	}
	return sub, nil
}

func preparationError(err error) error {
	if errors.Is(err, store.ErrLeaseNotOwned) {
		return ErrLeaseLost
	}
	return err
}
