package worker_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/worker"
)

// finishConcurrency bounds how many held executions complete at once after
// the gate opens. Completing all 512 in a single wave would storm the
// SQLite writer and burn the test deadline on busy timeouts rather than on
// the scheduling behavior under test.
const finishConcurrency = 8

// gatedExecutor records every submission ID it is asked to execute, then
// blocks until the test closes release. It lets the saturation test hold the
// prompt-started executions in place while the sweep delivers the rest. The
// first execution to reach the gate signals started, so the test can prove
// the hold is in effect before asserting on drops.
type gatedExecutor struct {
	mu       sync.Mutex
	observed map[string]bool
	calls    int
	started  chan struct{}
	release  chan struct{}
	finish   chan struct{}
}

func (g *gatedExecutor) Execute(ctx context.Context, sub *store.Submission) (contract.Result, error) {
	g.mu.Lock()
	if g.observed == nil {
		g.observed = make(map[string]bool)
	}
	g.observed[sub.ID] = true
	g.calls++
	g.mu.Unlock()
	select {
	case g.started <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
	case <-ctx.Done():
		return contract.Result{}, ctx.Err()
	}
	g.finish <- struct{}{} // bound the completion wave
	defer func() { <-g.finish }()
	return contract.Result{Content: []contract.ContentBlock{[]byte(`{"type":"text","text":"done"}`)}}, nil
}

func (g *gatedExecutor) distinct() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.observed)
}

func (g *gatedExecutor) observedID(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.observed[id]
}

func (g *gatedExecutor) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// TestSaturatedQueueStillExecutesEverySubmission pins the lossless-queue
// contract as a controlled regression:
//
//  1. the recurring sweep is pinned to a one-hour interval, so it cannot
//     schedule a test submission before or during the controlled burst;
//  2. a burst of more IDs than the queue depth (512 > 256) is offered
//     through Offer, which records the exact IDs the saturated queue
//     rejected;
//  3. one rejected ID is proven to remain durably runnable: accepted
//     status, present in the durable runnable set, never executed;
//  4. an explicit Service.Sweep — no process restart — schedules and
//     executes that same ID;
//  5. after the gate opens, every submission reaches completed, each
//     observed exactly once.
func TestSaturatedQueueStillExecutesEverySubmission(t *testing.T) {
	const count = 512 // twice the in-memory queue depth of 256

	path := filepath.Join(t.TempDir(), "state.db")
	keys := newMemoryKeys()
	st := openStore(t, path, keys)
	t.Cleanup(func() { _ = st.Close() })

	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("sub-sat-%03d", i)
	}

	exec := &gatedExecutor{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
		finish:  make(chan struct{}, finishConcurrency),
	}
	svc, err := worker.NewService(st, exec, worker.Config{
		Owner:    "worker-sat",
		LeaseTTL: 30 * time.Second,
		// Keep the recurring sweep out of the controlled window; the test
		// drives sweeps explicitly through svc.Sweep.
		SweepInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(svc.Stop)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Accept the work only after recovery has had nothing to recover, so no
	// submission can complete through the startup path.
	for _, id := range ids {
		createReplayable(t, st, id)
	}

	// Controlled burst: offer every ID without waiting and record exactly
	// which offers the saturated queue rejected.
	var rejected []string
	for _, id := range ids {
		if !svc.Offer(id) {
			rejected = append(rejected, id)
		}
	}
	if len(rejected) == 0 {
		t.Fatal("queue never saturated: no offer was rejected, the regression is not exercised")
	}
	t.Logf("%d prompt offers rejected by the saturated queue", len(rejected))

	// Establish the hold: at least one prompt-started execution is parked
	// on the gate, which the test keeps closed. No sweep can have run in
	// this window, so that execution came from a prompt offer.
	select {
	case <-exec.started:
	case <-time.After(10 * time.Second):
		t.Fatal("no prompt-started execution reached the gate")
	}

	// Pick the specific rejected ID and prove it is still durably
	// runnable: accepted, in the durable runnable set, never executed.
	target := rejected[0]
	sub, err := st.GetSubmission(context.Background(), target)
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if string(sub.Status) != "accepted" {
		t.Fatalf("rejected ID %s has status %s, want accepted", target, sub.Status)
	}
	if exec.observedID(target) {
		t.Fatalf("rejected ID %s was executed before any sweep ran", target)
	}
	runnable, err := st.ListRunnable(context.Background(), string(catalog.StrategyLocalReplayable))
	if err != nil {
		t.Fatalf("ListRunnable: %v", err)
	}
	listed := false
	for _, r := range runnable {
		if r == target {
			listed = true
			break
		}
	}
	if !listed {
		t.Fatalf("rejected ID %s is missing from the durable runnable set", target)
	}

	// A subsequent explicit sweep must schedule the same ID — no restart.
	deadline := time.Now().Add(30 * time.Second)
	for !exec.observedID(target) {
		if time.Now().After(deadline) {
			t.Fatalf("explicit sweeps never scheduled the rejected ID %s", target)
		}
		svc.Sweep(context.Background())
		time.Sleep(10 * time.Millisecond)
	}

	// Keep sweeping, as the recurring production sweep would, until every
	// rejected ID is scheduled and parked on the gate.
	deadline = time.Now().Add(60 * time.Second)
	for exec.distinct() < count {
		if time.Now().After(deadline) {
			t.Fatalf("explicit sweeps scheduled %d of %d submissions, want every one", exec.distinct(), count)
		}
		svc.Sweep(context.Background())
		time.Sleep(10 * time.Millisecond)
	}

	// Release the gate: every held execution completes under its lease.
	// As in production, the test keeps sweeping while it waits: a state
	// transition that loses a busy SQLite write leaves its submission
	// re-runnable, and the sweep re-drives it. A replayable submission may
	// therefore execute more than once, but never concurrently and never
	// unobserved.
	close(exec.release)
	deadline = time.Now().Add(300 * time.Second)
	for {
		allDone := true
		for _, id := range ids {
			sub, err := st.GetSubmission(context.Background(), id)
			if err != nil {
				t.Fatalf("GetSubmission: %v", err)
			}
			if string(sub.Status) != "completed" {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not every submission reached a terminal state without a restart (executed %d times)", exec.count())
		}
		svc.Sweep(context.Background())
		time.Sleep(50 * time.Millisecond)
	}
	if got := exec.count(); got < count {
		t.Fatalf("executor ran %d times, want at least %d (every submission executed)", got, count)
	}
	if got := exec.distinct(); got != count {
		t.Fatalf("executor observed %d distinct submissions, want %d", got, count)
	}
}
