package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/submission"
)

// The separate-process suite runs scenarios in this same test binary:
// TestMain dispatches on a private command argument, so each spawned process
// executes real OS-level work against one shared database.

const (
	envDB       = "STORE_PROCTEST_DB"
	envKey      = "STORE_PROCTEST_KEY"
	envSubID    = "STORE_PROCTEST_SUBID"
	envOwner    = "STORE_PROCTEST_OWNER"
	envTTLMS    = "STORE_PROCTEST_TTLMS"
	envLockMS   = "STORE_PROCTEST_LOCKMS"
	envGCSweeps = "STORE_PROCTEST_GC"
)

const scenarioArgPrefix = "--tama-link-store-scenario="

func TestMain(m *testing.M) {
	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, scenarioArgPrefix) {
			os.Exit(runScenario(strings.TrimPrefix(arg, scenarioArgPrefix)))
		}
	}
	os.Exit(m.Run())
}

// proctestFileKeys is a deterministic file-backed KeyProvider shared by
// every process in one scenario: the key is derived from the key path so
// concurrent first opens agree on the key material.
type proctestFileKeys struct {
	path string
}

func (f proctestFileKeys) GetStateKey(keyID string) ([]byte, bool, error) {
	if keyID != "shared" {
		return nil, false, nil
	}
	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func (f proctestFileKeys) CreateStateKey() (string, []byte, error) {
	derived := sha256.Sum256([]byte(f.path))
	err := os.WriteFile(f.path, derived[:], 0o600)
	return "shared", derived[:], err
}

func proctestOpen(t *testing.T, dbPath, keyPath string) *store.Store {
	t.Helper()

	s, err := store.Open(context.Background(), dbPath, proctestFileKeys{path: keyPath}, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// runProctest launches one scenario process and returns its exit code and
// combined output.
func runProctest(t *testing.T, name string, mutate func(*exec.Cmd)) (int, string) {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	cmd := exec.Command(exe, scenarioArgPrefix+name)
	cmd.Env = os.Environ()
	if mutate != nil {
		mutate(cmd)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		exit, ok := err.(*exec.ExitError)
		if ok {
			return exit.ExitCode(), string(out)
		}
		t.Fatalf("start scenario %s: %v\n%s", name, err, out)
	}
	return 0, string(out)
}

// withProctestEnv appends the scenario database and key paths to the command
// environment. The scenario selector is deliberately a command argument, so
// accidentally replacing the environment cannot recursively run the suite.
func withProctestEnv(dbPath, keyPath string, extra ...string) func(*exec.Cmd) {
	env := append([]string{envDB + "=" + dbPath, envKey + "=" + keyPath}, extra...)
	return func(cmd *exec.Cmd) { cmd.Env = append(cmd.Env, env...) }
}

// scenarioState is the shared paths for one scenario.
type scenarioState struct {
	db  string
	key string
}

func newScenarioState(t *testing.T) scenarioState {
	dir := t.TempDir()
	return scenarioState{db: filepath.Join(dir, "state.db"), key: filepath.Join(dir, "key")}
}

func runScenario(scenario string) int {
	db := os.Getenv(envDB)
	keys := proctestFileKeys{path: os.Getenv(envKey)}
	ctx := context.Background()

	s, err := store.Open(ctx, db, keys, store.Config{Limits: limits.Default()})
	if err != nil {
		fmt.Fprintf(os.Stderr, "open: %v\n", err)
		return 1
	}
	// Death scenarios exit explicitly and deliberately skip the close,
	// leaving the WAL file for the next opener to recover.
	defer func() { _ = s.Close() }()

	switch scenario {
	case "migrate":
		return 0

	case "submit-once":
		sub := store.NewSubmission{
			ID:               os.Getenv(envSubID),
			ClientRequestID:  "req-shared",
			Tool:             "message",
			Strategy:         "upstream_task",
			DescriptorDigest: "sha256:abc",
			Arguments:        []byte(`{"message":"hi"}`),
		}
		created, _, err := s.CreateSubmission(ctx, sub)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create: %v\n", err)
			return 1
		}
		fmt.Println(created.ID)
		return 0

	case "append-events-once":
		id := os.Getenv(envSubID)
		// Open first, then pause so a concurrent writer (the locker
		// process) can be live while this process's read-then-write
		// transaction runs. The overlap is the regression under test.
		time.Sleep(500 * time.Millisecond)
		event := contract.Event{
			SubmissionID: id,
			Sequence:     1,
			Timestamp:    time.Now().UTC(),
			State:        contract.StatusQueued,
		}
		if _, err := s.AppendEvents(ctx, id, []contract.Event{event}); err != nil {
			fmt.Fprintf(os.Stderr, "append events: %v\n", err)
			return 1
		}
		return 0

	case "lock":
		// Hold a raw write lock outside the store so another process must
		// wait out the busy timeout.
		raw, err := sql.Open("sqlite", "file:"+db+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
		if err != nil {
			fmt.Fprintf(os.Stderr, "raw open: %v\n", err)
			return 1
		}
		defer func() { _ = raw.Close() }()
		if _, err := raw.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			fmt.Fprintf(os.Stderr, "begin: %v\n", err)
			return 1
		}
		hold := 1500 * time.Millisecond
		if ms, perr := time.ParseDuration(os.Getenv(envLockMS) + "ms"); perr == nil && ms > 0 {
			hold = ms
		}
		time.Sleep(hold)
		if _, err := raw.ExecContext(ctx, "ROLLBACK"); err != nil {
			fmt.Fprintf(os.Stderr, "rollback: %v\n", err)
			return 1
		}
		return 0

	case "claim":
		owner := os.Getenv(envOwner)
		ttl, _ := time.ParseDuration(os.Getenv(envTTLMS) + "ms")
		if ttl <= 0 {
			ttl = 2 * time.Minute
		}
		owned, err := s.ClaimLease(ctx, "worker", owner, ttl)
		if err != nil {
			fmt.Fprintf(os.Stderr, "claim: %v\n", err)
			return 1
		}
		if owned {
			fmt.Println("owned")
		} else {
			fmt.Println("taken")
		}
		return 0

	case "renew":
		owner := os.Getenv(envOwner)
		renewed, err := s.RenewLease(ctx, "worker", owner, time.Minute)
		if err != nil {
			fmt.Fprintf(os.Stderr, "renew: %v\n", err)
			return 1
		}
		if renewed {
			fmt.Println("renewed")
		} else {
			fmt.Println("denied")
		}
		return 0

	case "claim-die":
		// Claim a lease and exit without releasing it, simulating process
		// death. The lease must remain durable and expire by the clock.
		owner := os.Getenv(envOwner)
		ttl, _ := time.ParseDuration(os.Getenv(envTTLMS) + "ms")
		if ttl <= 0 {
			ttl = 500 * time.Millisecond
		}
		owned, err := s.ClaimLease(ctx, "worker", owner, ttl)
		if err != nil || !owned {
			fmt.Fprintf(os.Stderr, "claim: %v\n", err)
			return 1
		}
		os.Exit(0)

	case "die":
		// Create a submission and exit without closing the store, so the
		// WAL file is left for the next opener to recover.
		if _, _, err := s.CreateSubmission(ctx, store.NewSubmission{
			ID: "sub-wal", ClientRequestID: "req-wal", Tool: "message",
			Strategy: "upstream_task", DescriptorDigest: "sha256:abc",
			Arguments: []byte(`{"message":"hi"}`),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "create: %v\n", err)
			return 1
		}
		os.Exit(0)

	case "capture-terminal":
		// Drive one submission to a completed result and leave it durable so a
		// later GC sweep must find it fresh (within retention) and keep it.
		subID := "sub-race"
		if _, _, err := s.CreateSubmission(ctx, store.NewSubmission{
			ID: subID, ClientRequestID: "req-race", Tool: "message",
			Strategy: "upstream_task", DescriptorDigest: "sha256:abc",
			Arguments: []byte(`{"message":"hi"}`),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "create: %v\n", err)
			return 1
		}
		for _, state := range []submission.State{
			submission.State(contract.StatusQueued),
			submission.State(contract.StatusRunning),
		} {
			if _, err := s.Transition(ctx, subID, state, store.TransitionDetail{}); err != nil {
				fmt.Fprintf(os.Stderr, "transition %s: %v\n", state, err)
				return 1
			}
		}
		result := contract.Result{Content: []contract.ContentBlock{[]byte(`{"type":"text","text":"done"}`)}}
		if _, err := s.Complete(ctx, subID, result); err != nil {
			fmt.Fprintf(os.Stderr, "capture: %v\n", err)
			return 1
		}
		return 0

	case "gc-sweeps":
		// Run a fixed number of GC sweeps against whatever is in the database.
		sweeps, _ := strconv.Atoi(os.Getenv(envGCSweeps))
		if sweeps <= 0 {
			sweeps = 5
		}
		for i := 0; i < sweeps; i++ {
			if _, err := s.GC(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "gc %d: %v\n", i, err)
				return 1
			}
		}
		return 0

	case "refresh":
		// Simulate one coordinated OAuth refresh: claim, work, release.
		owner := os.Getenv(envOwner)
		owned, err := s.ClaimLease(ctx, "oauth-refresh", owner, 30*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "claim: %v\n", err)
			return 1
		}
		if !owned {
			fmt.Println("not-owned")
			return 0
		}
		fmt.Println("owned")
		time.Sleep(100 * time.Millisecond)
		if err := s.ReleaseLease(ctx, "oauth-refresh", owner); err != nil {
			fmt.Fprintf(os.Stderr, "release: %v\n", err)
			return 1
		}
		fmt.Println("released")
		return 0

	case "integrity":
		if _, err := s.CheckIntegrity(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "integrity: %v\n", err)
			return 1
		}
		fmt.Println("ok")
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown scenario %q\n", scenario)
	return 1
}

func TestProctestConcurrentMigrations(t *testing.T) {
	st := newScenarioState(t)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, out := runProctest(t, "migrate", withProctestEnv(st.db, st.key))
			if code != 0 {
				errs[i] = fmt.Errorf("migrate %d exited %d: %s", i, code, out)
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent first open %d failed: %v", i, err)
		}
	}

	s := proctestOpen(t, st.db, st.key)
	if _, err := s.CheckIntegrity(context.Background()); err != nil {
		t.Fatalf("integrity after concurrent initialization: %v", err)
	}
}

func TestProctestCompetingIdempotentInserts(t *testing.T) {
	st := newScenarioState(t)

	ids := make([]string, 4)
	errs := make([]error, 4)
	var wg sync.WaitGroup
	for i := range ids {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, out := runProctest(t, "submit-once", withProctestEnv(st.db, st.key, envSubID+"=sub-"+strconv.Itoa(i)))
			ids[i] = strings.TrimSpace(out)
			if code != 0 {
				errs[i] = fmt.Errorf("submit %d exited %d: %s", i, code, out)
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("competing insert %d failed: %v", i, err)
		}
	}

	sort.Strings(ids)
	for i := range ids {
		if ids[i] != ids[0] {
			t.Fatalf("competing inserts returned different submissions: %v", ids)
		}
	}

	s := proctestOpen(t, st.db, st.key)
	got, err := s.GetSubmission(context.Background(), ids[0])
	if err != nil {
		t.Fatalf("winner lookup: %v", err)
	}
	if got.ClientRequestID != "req-shared" {
		t.Fatalf("client request id = %q", got.ClientRequestID)
	}
	// Only the winner's submission row survives; losers rolled back.
	for i := 0; i < 4; i++ {
		loser := "sub-" + strconv.Itoa(i)
		if loser == ids[0] {
			continue
		}
		if _, err := s.GetSubmission(context.Background(), loser); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("non-winner row %s survived: %v", loser, err)
		}
	}
}

func TestProctestBusyTimeout(t *testing.T) {
	st := newScenarioState(t)

	// The locker process holds a write lock for 1.5s. A concurrent
	// submission must wait (busy timeout) and then succeed instead of
	// failing.
	lockerDone := make(chan int, 1)
	go func() {
		code, _ := runProctest(t, "lock", withProctestEnv(st.db, st.key))
		lockerDone <- code
	}()
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	code, out := runProctest(t, "submit-once", withProctestEnv(st.db, st.key, envSubID+"=sub-busy"))
	if code != 0 {
		t.Fatalf("submit-once exited %d: %s", code, out)
	}
	if waited := time.Since(start); waited < time.Second {
		t.Fatalf("submission finished in %s, want it to wait out the held lock", waited)
	}
	if lockerCode := <-lockerDone; lockerCode != 0 {
		t.Fatalf("locker exited %d", lockerCode)
	}
}

// TestProctestWriteAfterReadWaitsForLock pins a driver behavior the store
// depends on: a write statement that follows a read inside one transaction
// must wait out the busy timeout while another process holds the write lock
// (for example inside a concurrent store open's validation window) instead of
// failing immediately with SQLITE_BUSY. All multi-statement write
// transactions therefore begin IMMEDIATE (see writeTx).
func TestProctestWriteAfterReadWaitsForLock(t *testing.T) {
	st := newScenarioState(t)
	s := proctestOpen(t, st.db, st.key)
	created, _, err := s.CreateSubmission(context.Background(), store.NewSubmission{
		ID:               "sub-write-after-read",
		ClientRequestID:  "war-1",
		Tool:             "message",
		Strategy:         "local_replayable",
		DescriptorDigest: "sha256:abc",
		Arguments:        []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create submission: %v", err)
	}

	// Start the appender first so its open completes unblocked; the locker
	// then holds the write lock across the appender's read-then-write
	// transaction.
	appenderDone := make(chan string, 1)
	go func() {
		code, out := runProctest(t, "append-events-once", withProctestEnv(st.db, st.key, envSubID+"="+created.ID))
		appenderDone <- out + fmt.Sprintf("(exit %d)", code)
	}()
	time.Sleep(100 * time.Millisecond)
	lockerCode, out := runProctest(t, "lock", withProctestEnv(st.db, st.key, envLockMS+"=4000"))
	if lockerCode != 0 {
		t.Fatalf("locker exited %d: %s", lockerCode, out)
	}
	result := <-appenderDone
	if strings.Contains(result, "(exit 0)") {
		return
	}
	t.Fatalf("append-events-once failed while a writer was held: %s", result)
}

func TestProctestExclusiveWorkerClaim(t *testing.T) {
	st := newScenarioState(t)

	code, out := runProctest(t, "claim", withProctestEnv(st.db, st.key, envOwner+"=owner-a"))
	if code != 0 || strings.TrimSpace(out) != "owned" {
		t.Fatalf("first claim = %q (exit %d), want owned", out, code)
	}
	code, out = runProctest(t, "claim", withProctestEnv(st.db, st.key, envOwner+"=owner-b"))
	if code != 0 || strings.TrimSpace(out) != "taken" {
		t.Fatalf("second claim = %q (exit %d), want taken", out, code)
	}
	code, out = runProctest(t, "renew", withProctestEnv(st.db, st.key, envOwner+"=owner-b"))
	if code != 0 || strings.TrimSpace(out) != "denied" {
		t.Fatalf("foreign renew = %q (exit %d), want denied", out, code)
	}
}

func TestProctestLeaseRecoveryAfterDeath(t *testing.T) {
	st := newScenarioState(t)

	code, out := runProctest(t, "claim-die", withProctestEnv(st.db, st.key,
		envOwner+"=owner-a", envTTLMS+"=400"))
	if code != 0 {
		t.Fatalf("claim-die exited %d: %s", code, out)
	}
	time.Sleep(600 * time.Millisecond)

	s := proctestOpen(t, st.db, st.key)
	owned, err := s.ClaimLease(context.Background(), "worker", "owner-b", time.Minute)
	if err != nil {
		t.Fatalf("ClaimLease: %v", err)
	}
	if !owned {
		t.Fatal("lease of a dead process was not recoverable after TTL")
	}
}

func TestProctestTerminalCaptureSurvivesGC(t *testing.T) {
	st := newScenarioState(t)

	// A terminal payload is captured durably by one process...
	code, out := runProctest(t, "capture-terminal", withProctestEnv(st.db, st.key))
	if code != 0 {
		t.Fatalf("capture-terminal exited %d: %s", code, out)
	}

	// ...then GC sweeps run by a second process must find the payload fresh
	// (within retention) and keep it, not expire it.
	code, out = runProctest(t, "gc-sweeps", withProctestEnv(st.db, st.key, envGCSweeps+"=5"))
	if code != 0 {
		t.Fatalf("gc-sweeps exited %d: %s", code, out)
	}

	s := proctestOpen(t, st.db, st.key)
	got, err := s.GetSubmission(context.Background(), "sub-race")
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if got.Status != submission.State(contract.StatusCompleted) {
		t.Fatalf("status = %s, want completed", got.Status)
	}
	if got.Result == nil || string(got.Result.Content[0]) != `{"type":"text","text":"done"}` {
		t.Fatal("GC wiped a fresh terminal payload")
	}
}

func TestProctestRefreshLeasing(t *testing.T) {
	st := newScenarioState(t)

	code, out := runProctest(t, "refresh", withProctestEnv(st.db, st.key, envOwner+"=owner-a"))
	if code != 0 {
		t.Fatalf("refresh exited %d: %s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] != "owned" || lines[1] != "released" {
		t.Fatalf("refresh transcript = %q, want owned then released", out)
	}

	s := proctestOpen(t, st.db, st.key)
	owned, err := s.ClaimLease(context.Background(), "oauth-refresh", "owner-b", time.Minute)
	if err != nil {
		t.Fatalf("ClaimLease: %v", err)
	}
	if !owned {
		t.Fatal("refresh lease was not released after the refresh finished")
	}
}

func TestProctestScenarioSurvivesEnvironmentReplacement(t *testing.T) {
	st := newScenarioState(t)
	code, out := runProctest(t, "integrity", func(cmd *exec.Cmd) {
		cmd.Env = []string{envDB + "=" + st.db, envKey + "=" + st.key}
	})
	if code != 0 {
		t.Fatalf("scenario exited %d after environment replacement: %s", code, out)
	}
}

func TestProctestWALRecovery(t *testing.T) {
	st := newScenarioState(t)

	code, out := runProctest(t, "die", withProctestEnv(st.db, st.key))
	if code != 0 {
		t.Fatalf("die exited %d: %s", code, out)
	}

	s := proctestOpen(t, st.db, st.key)
	got, err := s.GetSubmission(context.Background(), "sub-wal")
	if err != nil {
		t.Fatalf("submission missing after WAL recovery: %v", err)
	}
	if string(got.Arguments) != `{"message":"hi"}` {
		t.Fatalf("arguments = %s after WAL recovery", got.Arguments)
	}
	if _, err := s.CheckIntegrity(context.Background()); err != nil {
		t.Fatalf("integrity after WAL recovery: %v", err)
	}
}

func TestProctestIntegrityCheck(t *testing.T) {
	st := newScenarioState(t)

	code, out := runProctest(t, "integrity", withProctestEnv(st.db, st.key))
	if code != 0 || strings.TrimSpace(out) != "ok" {
		t.Fatalf("integrity scenario = %q (exit %d), want ok", out, code)
	}
}
