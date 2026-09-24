package application

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/worker"
)

const scenarioArgPrefix = "--tama-link-app-scenario="

func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, scenarioArgPrefix) {
			os.Exit(runScenario(strings.TrimPrefix(arg, scenarioArgPrefix)))
		}
	}
	m.Run()
}

func runScenario(name string) int {
	switch name {
	case "worker-single-winner":
		return workerSingleWinnerScenario()
	case "restart-replay-crash":
		return restartReplayCrashScenario()
	case "input-delivery":
		return inputDeliveryScenario()
	case "task-process":
		return taskProcessScenario()
	default:
		fmt.Fprintf(os.Stderr, "unknown scenario %q\n", name)
		return 2
	}
}

// appFileKeys is a deterministic file-backed KeyProvider shared by every
// process in one scenario.
type appFileKeys struct{ path string }

func (f appFileKeys) GetStateKey(keyID string) ([]byte, bool, error) {
	if keyID != "shared" {
		return nil, false, nil
	}
	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func (f appFileKeys) CreateStateKey() (string, []byte, error) {
	derived := sha256.Sum256([]byte(f.path))
	err := os.WriteFile(f.path, derived[:], 0o600)
	return "shared", derived[:], err
}

// recordingExecutor sleeps to widen the race window, records its process id
// as one execution line, and completes with a valid result. A crash executor
// records its start and then exits the process, modeling a crash mid-call.
type recordingExecutor struct {
	runsFile string
	hold     time.Duration
	crash    bool
}

func (r recordingExecutor) Execute(ctx context.Context, _ *store.Submission) (contract.Result, error) {
	select {
	case <-ctx.Done():
		return contract.Result{}, ctx.Err()
	case <-time.After(r.hold):
	}
	f, err := os.OpenFile(r.runsFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return contract.Result{}, err
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		_ = f.Close()
		return contract.Result{}, err
	}
	_ = f.Close()
	if r.crash {
		os.Exit(13)
	}
	return contract.Result{Content: []contract.ContentBlock{json.RawMessage(`{"type":"text","text":"done"}`)}}, nil
}

// workerSingleWinnerScenario runs inside one spawned process: open the shared
// store, recover, and let the durable lease decide the single winner.
func workerSingleWinnerScenario() int {
	dbPath := os.Getenv("APP_PROCTEST_DB")
	keyPath := os.Getenv("APP_PROCTEST_KEY")
	runsPath := os.Getenv("APP_PROCTEST_RUNS")
	if dbPath == "" || keyPath == "" || runsPath == "" {
		return 2
	}
	subID := os.Getenv("APP_PROCTEST_SUB")
	if subID == "" {
		subID = "sub-single-winner"
	}
	st, err := store.Open(context.Background(), dbPath, appFileKeys{path: keyPath}, store.Config{Limits: limits.Default()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "scenario open: %v\n", err)
		return 3
	}
	svc, err := worker.NewService(st, recordingExecutor{runsFile: runsPath, hold: 500 * time.Millisecond}, worker.Config{
		Owner:    fmt.Sprintf("scenario-%d", os.Getpid()),
		LeaseTTL: 30 * time.Second,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "scenario worker: %v\n", err)
		return 3
	}
	defer svc.Stop()
	_ = svc.Start(context.Background())
	// Both processes offered the one submission to their worker pools; the
	// durable lease decides the single winner. Wait for the completion
	// wave to drain before closing the store.
	deadline := time.Now().Add(30 * time.Second)
	for {
		s, err := st.GetSubmission(context.Background(), subID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scenario status: %v\n", err)
			return 4
		}
		if s.Status == contract.StatusCompleted || s.Status == contract.StatusFailed {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "scenario: submission never reached a terminal state\n")
			return 4
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = st.Close()
	return 0
}

// restartReplayCrashScenario runs inside one spawned process: recover the
// pending submission, record the execution start, and crash the process
// before completing so the durable lease expires with work unfinished.
func restartReplayCrashScenario() int {
	dbPath := os.Getenv("APP_PROCTEST_DB")
	keyPath := os.Getenv("APP_PROCTEST_KEY")
	runsPath := os.Getenv("APP_PROCTEST_RUNS")
	if dbPath == "" || keyPath == "" || runsPath == "" {
		return 2
	}
	st, err := store.Open(context.Background(), dbPath, appFileKeys{path: keyPath}, store.Config{Limits: limits.Default()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "scenario open: %v\n", err)
		return 3
	}
	svc, err := worker.NewService(st, recordingExecutor{runsFile: runsPath, hold: 2 * time.Second, crash: true}, worker.Config{
		Owner:    fmt.Sprintf("crash-%d", os.Getpid()),
		LeaseTTL: 3 * time.Second,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "scenario worker: %v\n", err)
		return 3
	}
	defer svc.Stop()
	_ = svc.Start(context.Background())
	// The pool executes the submitted work asynchronously; wait for the
	// crash executor to reach its record-and-exit point so the process
	// dies inside the upstream call, lease still held.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(runsPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "scenario: execution never recorded\n")
			return 4
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = st.Close()
	return 0
}

// TestRestartReplayReexecutesAfterLeaseExpiry proves that a crash mid-call
// leaves the submission durable and a new process re-executes it after the
// lease expires, completing it exactly once more.
func TestRestartReplayReexecutesAfterLeaseExpiry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := dir + "/state.db"
	keyPath := dir + "/key"
	runsPath := dir + "/runs"

	st, err := store.Open(context.Background(), dbPath, appFileKeys{path: keyPath}, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, _, err = st.CreateSubmission(context.Background(), store.NewSubmission{
		ID:               "sub-restart-replay",
		ClientRequestID:  "rr-1",
		Tool:             "status",
		Strategy:         "local_replayable",
		DescriptorDigest: "digest",
		Arguments:        json.RawMessage(`{}`),
		RequestArguments: json.RawMessage(`{}`),
		ProtocolVersion:  "2026-07-28",
		AdapterVersion:   "test",
	})
	if err != nil {
		t.Fatalf("create submission: %v", err)
	}
	_ = st.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	env := append(os.Environ(),
		"APP_PROCTEST_DB="+dbPath,
		"APP_PROCTEST_KEY="+keyPath,
		"APP_PROCTEST_RUNS="+runsPath,
	)

	// Phase 1: one process claims the submission, records the execution
	// start, and crashes before completing.
	crash := exec.Command(exe, scenarioArgPrefix+"restart-replay-crash")
	crash.Env = env
	out, runErr := crash.CombinedOutput()
	err = runErr
	if err == nil {
		t.Fatalf("crash scenario completed normally, want a mid-call crash\n%s", out)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 13 {
		t.Fatalf("crash scenario exit = %v, want code 13\n%s", err, out)
	}

	// The crash process's lease (TTL 3s from the claim) must expire before
	// the recovery process may re-execute the interrupted work.
	time.Sleep(4 * time.Second)

	// Phase 2: a new process recovers and completes the submission.
	replay := exec.Command(exe, scenarioArgPrefix+"worker-single-winner")
	replay.Env = append(env, "APP_PROCTEST_SUB=sub-restart-replay")
	replayOut, replayErr := replay.CombinedOutput()
	if replayErr != nil {
		t.Fatalf("replay process failed (exit %v):\n%s", replayErr, replayOut)
	}

	runs, err := os.ReadFile(runsPath)
	if err != nil {
		t.Fatalf("read executions: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(runs)), "\n")
	if len(lines) != 2 {
		t.Fatalf("execution lines = %d, want 2 (interrupted start plus replay):\n%s", len(lines), string(runs))
	}
	if lines[0] == lines[1] {
		t.Fatalf("both execution lines come from pid %s; the replay must come from the new process", lines[0])
	}

	st, err = store.Open(context.Background(), dbPath, appFileKeys{path: keyPath}, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st.Close() }()
	reloaded, err := st.GetSubmission(context.Background(), "sub-restart-replay")
	if err != nil {
		t.Fatalf("get submission: %v", err)
	}
	if reloaded.Status != contract.StatusCompleted {
		t.Fatalf("status = %s, want completed", reloaded.Status)
	}
}

// TestWorkerSingleWinnerAcrossProcesses proves two live processes contending
// for one pending submission execute it exactly once.
func TestWorkerSingleWinnerAcrossProcesses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := dir + "/state.db"
	keyPath := dir + "/key"
	runsPath := dir + "/runs"

	st, err := store.Open(context.Background(), dbPath, appFileKeys{path: keyPath}, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sub, _, err := st.CreateSubmission(context.Background(), store.NewSubmission{
		ID:               "sub-single-winner",
		ClientRequestID:  "cw-1",
		Tool:             "status",
		Strategy:         "local_replayable",
		DescriptorDigest: "digest",
		Arguments:        json.RawMessage(`{}`),
		RequestArguments: json.RawMessage(`{}`),
		ProtocolVersion:  "2026-07-28",
		AdapterVersion:   "test",
	})
	if err != nil {
		t.Fatalf("create submission: %v", err)
	}
	_ = st.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	cmds := make([]*exec.Cmd, 2)
	for i := range cmds {
		cmd := exec.Command(exe, scenarioArgPrefix+"worker-single-winner")
		cmd.Env = append(os.Environ(),
			"APP_PROCTEST_DB="+dbPath,
			"APP_PROCTEST_KEY="+keyPath,
			"APP_PROCTEST_RUNS="+runsPath,
		)
		cmds[i] = cmd
	}
	outputs := make([]string, 2)
	errs := make([]error, 2)
	done := make(chan struct{}, 2)
	for i, cmd := range cmds {
		go func(i int, cmd *exec.Cmd) {
			out, err := cmd.CombinedOutput()
			outputs[i] = string(out)
			errs[i] = err
			done <- struct{}{}
		}(i, cmd)
	}
	for range cmds {
		<-done
	}
	for i, err := range errs {
		if err != nil {
			var exit *exec.ExitError
			code := -1
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			}
			t.Fatalf("process %d failed (exit %d):\n%s", i+1, code, outputs[i])
		}
	}

	runs, err := os.ReadFile(runsPath)
	if err != nil {
		t.Fatalf("read executions: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(runs)), "\n")
	if len(lines) != 1 {
		t.Fatalf("submission executed %d times, want exactly 1:\n%s", len(lines), string(runs))
	}

	st, err = store.Open(context.Background(), dbPath, appFileKeys{path: keyPath}, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st.Close() }()
	reloaded, err := st.GetSubmission(context.Background(), sub.ID)
	if err != nil {
		t.Fatalf("get submission: %v", err)
	}
	if reloaded.Status != contract.StatusCompleted {
		t.Fatalf("status = %s, want completed", reloaded.Status)
	}
}
