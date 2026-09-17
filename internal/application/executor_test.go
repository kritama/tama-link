package application

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/contract"
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

// TestExecuteHonorsAcceptedResponseBound pins the per-submission response
// bound: an execution — including a recovered one — runs under the
// submission's accepted lifecycle policy, not the upstream client's
// currently configured bound. Raising the profile limit cannot admit a
// response the accepted policy rejects, and lowering it cannot fail a
// response the accepted policy allows.
func TestExecuteHonorsAcceptedResponseBound(t *testing.T) {
	t.Parallel()

	f := newFakeTama(t)

	// Raise: accepted under a 1 KiB bound, executed after the profile
	// raised to 64 KiB. The padded response exceeds the accepted bound,
	// so the execution fails even though the current client would allow
	// it.
	small := limits.Default()
	small.ResponseBytes = 1 * limits.KiB
	cfgSmall := fixtureConfigFor(t, f, small)
	svc, st, _ := appFromConfig(t, cfgSmall)
	id := submitStatus(t, svc, "raise-1")
	sub, err := st.GetSubmission(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if sub.AcceptedLimits.ResponseBytes != 1*limits.KiB {
		t.Fatalf("accepted response bound = %d, want 1024", sub.AcceptedLimits.ResponseBytes)
	}
	f.pad.Store(4096)

	raised := limits.Default()
	raised.ResponseBytes = 64 * limits.KiB
	cfgRaised := fixtureConfigFor(t, f, raised)
	ex := NewExecutor(cfgRaised.Connect)
	_, execErr := ex.Execute(context.Background(), sub)
	var be *BoundaryError
	if !errors.As(execErr, &be) || be.E.Code != contract.CodeResultTooLarge {
		t.Fatalf("Execute under the raised client bound = %v, want %s from the accepted bound", execErr, contract.CodeResultTooLarge)
	}

	// Lower: accepted under the 16 MiB default, executed after the
	// profile lowered to 1 KiB. The padded response fits the accepted
	// bound, so the execution succeeds even though the current client
	// would reject it.
	f.pad.Store(0)
	cfgBig := fixtureConfigFor(t, f, limits.Default())
	svcBig, stBig, _ := appFromConfig(t, cfgBig)
	id2 := submitStatus(t, svcBig, "lower-1")
	sub2, err := stBig.GetSubmission(context.Background(), id2)
	if err != nil {
		t.Fatalf("GetSubmission (lower): %v", err)
	}
	f.pad.Store(32 * 1024)

	lowered := limits.Default()
	lowered.ResponseBytes = 1 * limits.KiB
	cfgLowered := fixtureConfigFor(t, f, lowered)
	exLowered := NewExecutor(cfgLowered.Connect)
	if _, err := exLowered.Execute(context.Background(), sub2); err != nil {
		t.Fatalf("Execute under the lowered client bound = %v, want the accepted bound to govern", err)
	}
}
