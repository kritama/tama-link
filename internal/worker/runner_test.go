package worker_test

import (
	"context"
	"crypto/rand"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/worker"
)

type memoryKeys struct {
	mu   sync.Mutex
	keys map[string][]byte
}

func newMemoryKeys() *memoryKeys { return &memoryKeys{keys: make(map[string][]byte)} }

func (m *memoryKeys) GetStateKey(id string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, found := m.keys[id]
	return key, found, nil
}

func (m *memoryKeys) CreateStateKey() (string, []byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys["state"] = key
	return "state", key, nil
}

type executor struct {
	mu      sync.Mutex
	calls   int
	err     error
	started chan struct{}
	release chan struct{}
}

type losingState struct{ *store.Store }

func (l losingState) RenewLease(context.Context, string, string, time.Duration) (bool, error) {
	return false, nil
}

func (e *executor) Execute(ctx context.Context, _ *store.Submission) (contract.Result, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	if e.started != nil {
		select {
		case e.started <- struct{}{}:
		default:
		}
	}
	if e.release != nil {
		select {
		case <-e.release:
		case <-ctx.Done():
			return contract.Result{}, ctx.Err()
		}
	}
	if e.err != nil {
		return contract.Result{}, e.err
	}
	return contract.Result{Content: []contract.ContentBlock{[]byte(`{"type":"text","text":"done"}`)}}, nil
}

func (e *executor) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func openStore(t *testing.T, path string, keys *memoryKeys) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), path, keys, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func createReplayable(t *testing.T, s *store.Store, id string) {
	t.Helper()
	_, err := s.CreateSubmission(context.Background(), store.NewSubmission{
		ID: id, ClientRequestID: "request-" + id, Tool: "recall",
		Strategy: string(catalog.StrategyLocalReplayable), DescriptorDigest: "sha256:test",
		Arguments: []byte(`{"query":"memory"}`), ProtocolVersion: "2025-11-25", AdapterVersion: "tama014/1",
	})
	if err != nil {
		t.Fatalf("CreateSubmission: %v", err)
	}
}

func TestRecoverCompletesSubmissionAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	keys := newMemoryKeys()
	first := openStore(t, path, keys)
	createReplayable(t, first, "sub-1")
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openStore(t, path, keys)
	t.Cleanup(func() { _ = reopened.Close() })
	exec := &executor{}
	runner, err := worker.New(reopened, exec, worker.Config{Owner: "worker-a", LeaseTTL: time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := runner.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got, err := reopened.GetSubmission(context.Background(), "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if got.Status != contract.StatusCompleted || got.Result == nil || exec.count() != 1 {
		t.Fatalf("recovered submission = status %s, result %v, calls %d", got.Status, got.Result, exec.count())
	}
}

func TestExecutionFailureBecomesTerminal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openStore(t, path, newMemoryKeys())
	t.Cleanup(func() { _ = s.Close() })
	createReplayable(t, s, "sub-1")
	runner, err := worker.New(s, &executor{err: errors.New("private upstream failure")}, worker.Config{
		Owner: "worker-a", LeaseTTL: time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := runner.Run(context.Background(), "sub-1"); err == nil {
		t.Fatal("Run succeeded, want execution error")
	}
	got, err := s.GetSubmission(context.Background(), "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if got.Status != contract.StatusFailed || got.ErrorCode != string(contract.CodeUpstreamExecutionFailed) {
		t.Fatalf("failed submission = %+v", got)
	}
	if got.ErrorMessage == "private upstream failure" {
		t.Fatal("private executor error leaked into durable client-facing state")
	}
}

func TestConcurrentRunnerCannotDuplicateWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openStore(t, path, newMemoryKeys())
	t.Cleanup(func() { _ = s.Close() })
	createReplayable(t, s, "sub-1")
	exec := &executor{started: make(chan struct{}, 1), release: make(chan struct{})}
	first, _ := worker.New(s, exec, worker.Config{Owner: "worker-a", LeaseTTL: time.Second})
	second, _ := worker.New(s, exec, worker.Config{Owner: "worker-b", LeaseTTL: time.Second})
	done := make(chan error, 1)
	go func() { done <- first.Run(context.Background(), "sub-1") }()
	<-exec.started
	if err := second.Run(context.Background(), "sub-1"); !errors.Is(err, worker.ErrLeaseHeld) {
		t.Fatalf("second Run = %v, want ErrLeaseHeld", err)
	}
	close(exec.release)
	if err := <-done; err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if exec.count() != 1 {
		t.Fatalf("executor calls = %d, want 1", exec.count())
	}
}

func TestConcurrentRunOnSameRunnerCannotDuplicateWork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openStore(t, path, newMemoryKeys())
	t.Cleanup(func() { _ = s.Close() })
	createReplayable(t, s, "sub-1")
	exec := &executor{started: make(chan struct{}, 1), release: make(chan struct{})}
	runner, _ := worker.New(s, exec, worker.Config{Owner: "worker-a", LeaseTTL: time.Second})
	done := make(chan error, 1)
	go func() { done <- runner.Run(context.Background(), "sub-1") }()
	<-exec.started
	if err := runner.Run(context.Background(), "sub-1"); !errors.Is(err, worker.ErrLeaseHeld) {
		t.Fatalf("reentrant Run = %v, want ErrLeaseHeld", err)
	}
	close(exec.release)
	if err := <-done; err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if exec.count() != 1 {
		t.Fatalf("executor calls = %d, want 1", exec.count())
	}
}

func TestCancelledRunRemainsRecoverable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openStore(t, path, newMemoryKeys())
	t.Cleanup(func() { _ = s.Close() })
	createReplayable(t, s, "sub-1")

	blocking := &executor{started: make(chan struct{}, 1), release: make(chan struct{})}
	first, _ := worker.New(s, blocking, worker.Config{Owner: "worker-a", LeaseTTL: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- first.Run(ctx, "sub-1") }()
	<-blocking.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Run = %v, want context.Canceled", err)
	}
	interrupted, err := s.GetSubmission(context.Background(), "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if interrupted.Status != contract.StatusRunning {
		t.Fatalf("interrupted status = %s, want running", interrupted.Status)
	}

	replay := &executor{}
	second, _ := worker.New(s, replay, worker.Config{Owner: "worker-b", LeaseTTL: time.Second})
	if err := second.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	completed, err := s.GetSubmission(context.Background(), "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission after recovery: %v", err)
	}
	if completed.Status != contract.StatusCompleted || replay.count() != 1 {
		t.Fatalf("recovered status = %s, calls = %d", completed.Status, replay.count())
	}
}

func TestLeaseLossCancelsExecution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openStore(t, path, newMemoryKeys())
	t.Cleanup(func() { _ = s.Close() })
	createReplayable(t, s, "sub-1")
	blocking := &executor{release: make(chan struct{})}
	runner, err := worker.New(losingState{s}, blocking, worker.Config{Owner: "worker-a", LeaseTTL: 30 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := runner.Run(context.Background(), "sub-1"); !errors.Is(err, worker.ErrLeaseLost) {
		t.Fatalf("Run after lease loss = %v, want ErrLeaseLost", err)
	}
	got, err := s.GetSubmission(context.Background(), "sub-1")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if got.Status != contract.StatusRunning || got.Result != nil {
		t.Fatalf("lease-lost submission = %+v, want recoverable running state", got)
	}
}
