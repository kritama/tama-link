package worker_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/worker"
)

// TestSaturatedQueueStillExecutesEverySubmission proves the scheduling
// guarantee: when the in-memory dispatch queue is saturated, the dropped IDs
// are rediscovered by the recurring durable sweep and every accepted
// replayable submission reaches a terminal state without a process restart.
func TestSaturatedQueueStillExecutesEverySubmission(t *testing.T) {
	const count = 300 // above the in-memory queue depth of 256

	path := filepath.Join(t.TempDir(), "state.db")
	keys := newMemoryKeys()
	st := openStore(t, path, keys)
	t.Cleanup(func() { _ = st.Close() })
	for i := 0; i < count; i++ {
		createReplayable(t, st, fmt.Sprintf("sub-sat-%03d", i))
	}

	exec := &executor{}
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

	// Offer every submission without waiting: the queue saturates and the
	// non-blocking sends drop the overflow.
	for i := 0; i < count; i++ {
		svc.Dispatch(fmt.Sprintf("sub-sat-%03d", i))
	}

	// Every submission must land in a terminal completed state without a
	// restart. A transient store failure between execution and completion
	// gives the lease back and the sweep replays the read-only work, so the
	// executor count is at-least-once by design; the lease still bounds
	// concurrent execution.
	deadline := time.Now().Add(90 * time.Second)
	for {
		allDone := true
		for i := 0; i < count; i++ {
			sub, err := st.GetSubmission(context.Background(), fmt.Sprintf("sub-sat-%03d", i))
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
		time.Sleep(100 * time.Millisecond)
	}
	if got := exec.count(); got < count {
		t.Fatalf("executor ran %d times, want at least %d", got, count)
	}
}
