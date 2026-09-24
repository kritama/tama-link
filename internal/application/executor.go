package application

import (
	"context"
	"fmt"
	"sync"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/upstream"
	"github.com/kritama/tama-link/internal/worker"
)

// Executor executes local_replayable submissions through one verified
// connection, resolving it on first use and reusing it for the process
// lifetime. It implements worker.Executor and maps adapter failures to
// stable contract errors so the worker records the exact boundary taxonomy
// on the submission.
type Executor struct {
	conns verifiedConnection
}

// MemoConnect returns a resolver that calls resolve once and reuses the
// verified connection. A failed resolve is not cached. System execution and
// the App task runner must share one memoized resolver so discovery is not
// repeated per submission.
func MemoConnect(resolve func(ctx context.Context) (*tama2026.Connection, error)) func(context.Context) (*tama2026.Connection, error) {
	if resolve == nil {
		panic("application: connection resolver is required")
	}
	shared := &verifiedConnection{resolve: resolve}
	return shared.get
}

// NewExecutor builds the System-path worker executor over one connection
// resolver. The resolver may establish the connection lazily; its error is
// classified before the submission fails.
func NewExecutor(resolve func(ctx context.Context) (*tama2026.Connection, error)) *Executor {
	if resolve == nil {
		panic("application: executor connection resolver is required")
	}
	return &Executor{conns: verifiedConnection{resolve: resolve}}
}

// verifiedConnection memoizes the first successful resolved connection for
// the process lifetime. The documented contract of the connection resolver
// is that the application calls it once and reuses the result: re-resolving
// per execution would repeat the authenticated server/discover and the
// complete paginated tools/list for every locally replayable operation and
// let a transient discovery outage fail already queued work. The token
// provider inside the connection still tracks refreshes, so a reused
// connection always authenticates with the current credential. Failures
// are never cached: a failed resolve fails the current operation and the
// next one retries.
type verifiedConnection struct {
	mu      sync.Mutex
	resolve func(ctx context.Context) (*tama2026.Connection, error)
	conn    *tama2026.Connection
}

func (v *verifiedConnection) get(ctx context.Context) (*tama2026.Connection, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.conn != nil {
		return v.conn, nil
	}
	cn, err := v.resolve(ctx)
	if err != nil {
		return nil, err
	}
	v.conn = cn
	return cn, nil
}

// Execute runs one ordinary synchronous tools/call for a replayable
// submission. The implementation is safe to replay: the pinned operation is
// read-only or idempotent and the lease guarantees a single live execution.
// Before the upstream call, the submission's accepted descriptor digest is
// rechecked against the connection's effective descriptor: if the profile
// was reconciled to a different descriptor under the same tool name while
// the submission was durably pending, the old arguments are not executed
// under the new contract.
func (e *Executor) Execute(ctx context.Context, sub *store.Submission) (contract.Result, error) {
	cn, err := e.conns.get(ctx)
	if err != nil {
		if deferredErr := deferredIfContended(err); deferredErr != nil {
			return contract.Result{}, deferredErr
		}
		return contract.Result{}, &BoundaryError{E: *classify(err)}
	}
	if err := checkDescriptorDigest(cn, sub); err != nil {
		return contract.Result{}, &BoundaryError{E: *err}
	}
	// The submission's accepted response bound governs this execution: a
	// replayable submission recovered after a profile limit change runs
	// under the policy it was accepted with, not the profile's current
	// value.
	result, err := cn.ExecuteLocal(ctx, sub.Tool, sub.Arguments, int64(sub.AcceptedLimits.ResponseBytes))
	if err != nil {
		if deferredErr := deferredIfContended(err); deferredErr != nil {
			return contract.Result{}, deferredErr
		}
		return contract.Result{}, &BoundaryError{E: *classify(err)}
	}
	return *result, nil
}

// deferredIfContended maps refresh-lease contention to a deferred execution:
// another process owns the refresh lease and is refreshing the same valid
// credential, so the work must be redelivered by the sweep rather than
// failed as an authentication error.
func deferredIfContended(err error) error {
	if !upstream.IsTokenContended(err) {
		return nil
	}
	return fmt.Errorf("%w: refresh lease contended in another process", worker.ErrExecutionDeferred)
}

// checkDescriptorDigest verifies that the submission's accepted descriptor
// still matches the effective catalog. A submission that was accepted under
// one descriptor and replayed under a different one (same tool name) would
// execute stale arguments under a new contract.
func checkDescriptorDigest(cn *tama2026.Connection, sub *store.Submission) *contract.Error {
	d, ok := cn.Catalog().Find(sub.Tool)
	if !ok || d.Digest != sub.DescriptorDigest {
		e := contract.NewError(contract.CodeOperationContractMismatch,
			"The accepted operation no longer matches its pinned contract.")
		return &e
	}
	return nil
}
