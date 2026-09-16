package application

import (
	"context"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
)

// Executor executes local_replayable submissions through one verified
// connection, resolving it on first use. It implements worker.Executor and
// maps adapter failures to stable contract errors so the worker records the
// exact boundary taxonomy on the submission.
type Executor struct {
	resolve func(ctx context.Context) (*tama2026.Connection, error)
}

// NewExecutor builds the System-path worker executor over one connection
// resolver. The resolver may establish the connection lazily; its error is
// classified before the submission fails.
func NewExecutor(resolve func(ctx context.Context) (*tama2026.Connection, error)) *Executor {
	if resolve == nil {
		panic("application: executor connection resolver is required")
	}
	return &Executor{resolve: resolve}
}

// Execute runs one ordinary synchronous tools/call for a replayable
// submission. The implementation is safe to replay: the pinned operation is
// read-only or idempotent and the lease guarantees a single live execution.
func (e *Executor) Execute(ctx context.Context, sub *store.Submission) (contract.Result, error) {
	cn, err := e.resolve(ctx)
	if err != nil {
		return contract.Result{}, &BoundaryError{E: *classify(err)}
	}
	result, err := cn.ExecuteLocal(ctx, sub.Tool, sub.Arguments)
	if err != nil {
		return contract.Result{}, &BoundaryError{E: *classify(err)}
	}
	return *result, nil
}
