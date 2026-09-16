package worker_test

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
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
	active   int
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
	g.active++
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.active--
		g.mu.Unlock()
	}()
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

func (g *gatedExecutor) activeCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active
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
//     executes that same ID: no other path could have delivered it;
//  5. sweeping continues until every submission reaches completed.
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

	// No gate: executions complete as soon as they start, so the test
	// measures scheduling against the durable store, not against held
	// executions. A bounded worker pool means only a few executions are
	// live at once; the sweep keeps delivering the rest.
	exec := &executor{}
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

	// Pick the specific rejected ID and prove it is still durably
	// runnable: accepted, in the durable runnable set, never completed.
	// No sweep can have run in this window, and the prompt offer was
	// rejected, so the ID could not have been executed.
	target := rejected[0]
	sub, err := st.GetSubmission(context.Background(), target)
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if string(sub.Status) != "accepted" {
		t.Fatalf("rejected ID %s has status %s, want accepted", target, sub.Status)
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

	// A subsequent explicit sweep must schedule and execute the same ID —
	// no restart, and no prompt path that could deliver it.
	deadline := time.Now().Add(60 * time.Second)
	for {
		sub, err := st.GetSubmission(context.Background(), target)
		if err != nil {
			t.Fatalf("GetSubmission: %v", err)
		}
		if string(sub.Status) == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("explicit sweeps never scheduled the rejected ID %s", target)
		}
		svc.Sweep(context.Background())
		time.Sleep(10 * time.Millisecond)
	}

	// Keep sweeping, as the production recurring sweep would, until every
	// submission reaches completed. A transition that loses a busy SQLite
	// write leaves its submission re-runnable, and the sweep re-drives it.
	deadline = time.Now().Add(120 * time.Second)
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
		time.Sleep(20 * time.Millisecond)
	}
	if got := exec.count(); got < count {
		t.Fatalf("executor ran %d times, want at least %d (every submission executed)", got, count)
	}
}

// TestInFlightExecutionsAreBounded pins the worker's concurrency contract:
// no matter how many IDs the queue or a sweep offers, at most MaxInFlight
// submissions execute at once; the rest wait for a free slot and still
// complete.
func TestInFlightExecutionsAreBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	keys := newMemoryKeys()
	st := openStore(t, path, keys)
	t.Cleanup(func() { _ = st.Close() })

	exec := &gatedExecutor{started: make(chan struct{}, 1), release: make(chan struct{}), finish: make(chan struct{}, finishConcurrency)}
	svc, err := worker.NewService(st, exec, worker.Config{
		Owner:         "worker-bounded",
		LeaseTTL:      30 * time.Second,
		SweepInterval: time.Hour,
		MaxInFlight:   3,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(svc.Stop)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const count = 6
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("sub-bounded-%d", i)
		createReplayable(t, st, id)
		if !svc.Offer(id) {
			t.Fatalf("offer %d rejected on an empty queue", i)
		}
	}

	// Wait until the bound is reached; the gate holds every execution.
	deadline := time.Now().Add(10 * time.Second)
	for exec.activeCount() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d executions reached the executor, want the bound of 3", exec.activeCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Give any (impossible) over-bound execution time to show up.
	time.Sleep(100 * time.Millisecond)
	if got := exec.activeCount(); got > 3 {
		t.Fatalf("%d executions ran concurrently, want at most 3", got)
	}

	close(exec.release)
	deadline = time.Now().Add(30 * time.Second)
	for {
		done := 0
		for i := 0; i < count; i++ {
			sub, err := st.GetSubmission(context.Background(), fmt.Sprintf("sub-bounded-%d", i))
			if err != nil {
				t.Fatalf("GetSubmission: %v", err)
			}
			if string(sub.Status) == "completed" {
				done++
			}
		}
		if done == count {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d bounded submissions completed", done, count)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestParkedWaitersAreBounded pins the memory contract of the bounded
// pool: while the pool is saturated, dispatched-but-not-started work waits
// in a bounded parking set — never one goroutine per offered ID — and the
// excess stays durable and sweep-redelivered.
func TestParkedWaitersAreBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	keys := newMemoryKeys()
	st := openStore(t, path, keys)
	t.Cleanup(func() { _ = st.Close() })

	const maxInFlight = 2
	exec := &gatedExecutor{started: make(chan struct{}, 1), release: make(chan struct{}), finish: make(chan struct{}, finishConcurrency)}
	svc, err := worker.NewService(st, exec, worker.Config{
		Owner:         "worker-parked",
		LeaseTTL:      30 * time.Second,
		SweepInterval: time.Hour,
		MaxInFlight:   maxInFlight,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(svc.Stop)
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const count = 32
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("sub-parked-%02d", i)
		createReplayable(t, st, ids[i])
	}

	baseline := runtime.NumGoroutine()
	for _, id := range ids {
		svc.Offer(id)
	}
	// Wait until the pool is held behind the gate.
	deadline := time.Now().Add(10 * time.Second)
	for exec.activeCount() < maxInFlight {
		if time.Now().After(deadline) {
			t.Fatalf("only %d executions reached the executor, want %d", exec.activeCount(), maxInFlight)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Let the dispatch loop drain the queue and spawn every waiter it will.
	time.Sleep(250 * time.Millisecond)
	if extra := runtime.NumGoroutine() - baseline; extra > 2*maxInFlight+4 {
		t.Fatalf("dispatch spawned %d goroutines for a held pool of %d; waiters must be bounded by the pool", extra, maxInFlight)
	}

	close(exec.release)
	deadline = time.Now().Add(60 * time.Second)
	for {
		done := 0
		for _, id := range ids {
			sub, err := st.GetSubmission(context.Background(), id)
			if err != nil {
				t.Fatalf("GetSubmission: %v", err)
			}
			if string(sub.Status) == "completed" {
				done++
			}
		}
		if done == count {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d submissions completed; the unmarked excess must be sweep-redelivered", done, count)
		}
		svc.Sweep(context.Background())
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStartRecoversWithoutBlockingServing pins the startup contract: Start
// lists the durable backlog and offers it to the bounded pool, then returns
// immediately — even while every execution slot is held by an upstream
// call — so a large backlog cannot keep the MCP server from accepting
// clients.
func TestStartRecoversWithoutBlockingServing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	keys := newMemoryKeys()
	st := openStore(t, path, keys)
	t.Cleanup(func() { _ = st.Close() })

	// The executor holds every execution; with two runnable rows and a
	// pool of two, the whole pool is blocked behind the gate.
	exec := &gatedExecutor{started: make(chan struct{}, 2), release: make(chan struct{}), finish: make(chan struct{}, finishConcurrency)}
	svc, err := worker.NewService(st, exec, worker.Config{
		Owner:         "worker-start",
		LeaseTTL:      30 * time.Second,
		SweepInterval: time.Hour,
		MaxInFlight:   2,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(svc.Stop)

	createReplayable(t, st, "sub-start-1")
	createReplayable(t, st, "sub-start-2")

	// Start must return while the pool is blocked behind the gate.
	started := make(chan error, 1)
	go func() { started <- svc.Start(context.Background()) }()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start blocked on the execution pool; recovery must not delay serving")
	}

	// The backlog is in flight behind the gate: both rows were offered and
	// are executing. Release and drain.
	deadline := time.Now().Add(10 * time.Second)
	for exec.activeCount() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d executions started, want both backlog rows in flight", exec.activeCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(exec.release)
	deadline = time.Now().Add(30 * time.Second)
	for {
		done := true
		for _, id := range []string{"sub-start-1", "sub-start-2"} {
			sub, err := st.GetSubmission(context.Background(), id)
			if err != nil {
				t.Fatalf("GetSubmission: %v", err)
			}
			if string(sub.Status) != "completed" {
				done = false
				break
			}
		}
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backlog did not drain after the gate released")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
