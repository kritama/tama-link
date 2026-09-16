package worker_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/worker"
)

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
	return contract.Result{Content: []contract.ContentBlock{[]byte(`{"type":"text","text":"done"}`)}}, nil
}

func (g *gatedExecutor) distinct() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.observed)
}

func (g *gatedExecutor) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// TestSaturatedQueueStillExecutesEverySubmission proves the scheduling
// guarantee end to end without a restart:
//
//  1. the service starts before any submission exists, so startup recovery
//     has nothing to do;
//  2. a burst of more IDs than the queue depth (512 > 256) is offered to
//     Dispatch, and the test asserts at least one prompt dispatch was
//     actually dropped by the saturated queue;
//  3. while every prompt-started execution is held on a gate, the test
//     waits until the executor has observed every one of the 512 IDs. A
//     dropped ID never entered the queue through Dispatch, and the
//     in-flight guard absorbs duplicate offers, so only the recurring
//     durable sweep could have delivered it; and
//  4. after the gate opens, every submission reaches completed.
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

	exec := &gatedExecutor{started: make(chan struct{}, 1), release: make(chan struct{})}
	svc, err := worker.NewService(st, exec, worker.Config{
		Owner:         "worker-sat",
		LeaseTTL:      30 * time.Second,
		SweepInterval: 50 * time.Millisecond,
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

	// Offer every submission without waiting. The tight burst outruns the
	// dispatch loop's drain by a wide margin, so a saturated queue must
	// drop at least one prompt dispatch; the counter makes that observable.
	for _, id := range ids {
		svc.Dispatch(id)
	}

	// Establish the hold before asserting on drops: at least one
	// prompt-started execution must be parked on the gate, which the test
	// keeps closed. While the gate is held, no prompt-started execution can
	// complete, so nothing below can be satisfied by the prompt path alone.
	select {
	case <-exec.started:
	case <-time.After(10 * time.Second):
		t.Fatal("no prompt-started execution reached the gate")
	}

	if drops := svc.DroppedDispatches(); drops < 1 {
		t.Fatalf("queue never saturated: %d dispatches dropped, want at least one", drops)
	}
	t.Logf("%d prompt dispatches dropped by the saturated queue", svc.DroppedDispatches())

	// While the gate holds every prompt-started execution, wait until the
	// executor has observed all IDs. Dispatch accepted exactly count-drops
	// distinct IDs, so every additional observation must have been
	// scheduled by the recurring durable sweep, not by the prompt queue.
	deadline := time.Now().Add(60 * time.Second)
	for exec.distinct() < count {
		if time.Now().After(deadline) {
			t.Fatalf("sweep observed %d of %d submissions, want every one", exec.distinct(), count)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Release the gate: every held execution completes under its lease.
	close(exec.release)
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
		time.Sleep(50 * time.Millisecond)
	}
	if got := exec.count(); got < count {
		t.Fatalf("executor ran %d times, want at least %d", got, count)
	}
}
