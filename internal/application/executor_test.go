package application

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/limits"
)

// TestExecutorReusesVerifiedConnection pins the connection contract: the
// first successful resolve is cached for the process lifetime, so the
// authenticated server/discover and the complete paginated tools/list run
// once — not once per locally replayable operation.
func TestExecutorReusesVerifiedConnection(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)
	cfg := fixtureConfigFor(t, f, limits.Default())
	svc, st, _ := appFromConfig(t, cfg)

	id := submitStatus(t, svc, "conn-1")
	sub, err := st.GetSubmission(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}

	var calls int32
	connect := func(ctx context.Context) (*tama2026.Connection, error) {
		atomic.AddInt32(&calls, 1)
		return cfg.Connect(ctx)
	}
	ex := NewExecutor(connect)

	for i := 0; i < 2; i++ {
		if _, err := ex.Execute(context.Background(), sub); err != nil {
			t.Fatalf("Execute %d: %v", i+1, err)
		}
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("connection resolver called %d times across two executions, want 1", got)
	}
}
