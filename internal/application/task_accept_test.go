package application

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/upstream"
	"github.com/kritama/tama-link/internal/worker"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTaskRestartCrossesProcessBoundary(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.onGet = func(int) (int, string) {
		return http.StatusOK, taskState("working", "2026-09-11T10:00:02Z", "")
	}
	dir := t.TempDir()
	dbPath := dir + "/state.db"
	keyPath := dir + "/key"
	pidPath := dir + "/pids"
	subPath := dir + "/sub"
	if err := os.WriteFile(pidPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := processEnv(dbPath, keyPath, up.ts.URL+"/mcp/app", pidPath, subPath, "restart", "proven")
	if code, out := runProcess(t, env, "a"); code != 0 {
		t.Fatalf("process A exit %d\n%s", code, out)
	}
	if up.callCount() != 1 || up.graphCount() != 1 || up.authCount() == 0 {
		t.Fatalf("after A calls=%d graph=%d auth=%d", up.callCount(), up.graphCount(), up.authCount())
	}
	up.onGet = func(int) (int, string) {
		return http.StatusOK, taskState("completed", "2026-09-11T10:00:08Z", taskSuccessResult(false))
	}
	getsBefore := up.getCount()
	obsBefore := len(up.observations())
	if code, out := runProcess(t, env, "b"); code != 0 {
		t.Fatalf("process B exit %d\n%s", code, out)
	}
	if up.callCount() != 1 || up.graphCount() != 1 || up.getCount() <= getsBefore {
		t.Fatalf("after B calls=%d graph=%d gets=%d", up.callCount(), up.graphCount(), up.getCount())
	}
	if !authenticatedLookup(up.observations()[obsBefore:]) {
		t.Fatal("process B did not perform a fresh authenticated tasks/get")
	}
	pids := readPIDs(t, pidPath)
	if len(pids) != 2 || pids[0] == pids[1] {
		t.Fatalf("pids = %v", pids)
	}
	up.ts.Close()
	subID := readTrim(t, dir+"/sub")
	awaited := awaitFromReopenedStore(t, dbPath, keyPath, up.ts.URL+"/mcp/app", subID)
	if !awaited.Terminal || awaited.Result == nil || awaited.Status != contract.StatusCompleted {
		t.Fatalf("retained await = %+v", awaited)
	}
	if awaited.Result.IsError {
		t.Fatal("retained await returned an error result")
	}
}

func TestAmbiguousAcceptanceDoesNotDuplicateGraphWork(t *testing.T) {
	t.Parallel()

	t.Run("proven binding replays the same task", func(t *testing.T) {
		t.Parallel()
		assertAmbiguous(t, "proven", contract.StatusCompleted, 2, 1)
	})
	t.Run("unproven binding does not replay", func(t *testing.T) {
		t.Parallel()
		assertAmbiguous(t, "unproven", contract.StatusOutcomeUnknown, 1, 1)
	})
}

func assertAmbiguous(t *testing.T, binding string, want contract.Status, wantCalls, wantGraph int) {
	t.Helper()
	up := newTaskUpstream(t)
	up.dropFirst.Store(true)
	up.onGet = func(int) (int, string) {
		return http.StatusOK, taskState("completed", "2026-09-11T10:00:08Z", taskSuccessResult(false))
	}
	dir := t.TempDir()
	dbPath := dir + "/state.db"
	keyPath := dir + "/key"
	pidPath := dir + "/pids"
	subPath := dir + "/sub"
	if err := os.WriteFile(pidPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := processEnv(dbPath, keyPath, up.ts.URL+"/mcp/app", pidPath, subPath, "ambiguous", binding)
	if code, out := runProcess(t, env, "a"); code != 0 {
		t.Fatalf("process A exit %d\n%s", code, out)
	}
	if up.graphCount() != 1 || up.callCount() != 1 {
		t.Fatalf("after dropped call calls=%d graph=%d", up.callCount(), up.graphCount())
	}
	if code, out := runProcess(t, env, "b"); code != 0 {
		t.Fatalf("process B exit %d\n%s", code, out)
	}
	if up.callCount() != wantCalls || up.graphCount() != wantGraph {
		t.Fatalf("calls=%d graph=%d, want %d/%d", up.callCount(), up.graphCount(), wantCalls, wantGraph)
	}
	if wantCalls == 2 {
		calls := up.callIdentities()
		if len(calls) != 2 || calls[0] != calls[1] {
			t.Fatalf("replay is not the same canonical call: %#v", calls)
		}
	}
	st, err := store.Open(context.Background(), dbPath, appFileKeys{path: keyPath}, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	sub, err := st.GetSubmission(context.Background(), readTrim(t, dir+"/sub"))
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != want {
		t.Fatalf("status = %s, want %s", sub.Status, want)
	}
}

func authenticatedLookup(obs []authObservation) bool {
	for _, item := range obs {
		if item.Method == "tasks/get" && item.Authorized {
			return true
		}
	}
	return false
}

func awaitFromReopenedStore(t *testing.T, dbPath, keyPath, endpoint, subID string) contract.AwaitOutput {
	t.Helper()
	st, err := store.Open(context.Background(), dbPath, appFileKeys{path: keyPath}, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	p, err := newFixtureProfile(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	var upstreamCalls int
	connect := func(context.Context) (*tama2026.Connection, error) {
		upstreamCalls++
		return nil, errUpstreamCalled
	}
	workerService, err := worker.NewService(st, NewExecutor(connect), worker.Config{Owner: "reopen", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(workerService.Stop)
	tasks, err := NewTaskService(st, connect, TaskConfig{Owner: "reopen-tasks", LeaseTTL: time.Minute, SweepInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tasks.Stop)
	svc, err := New(Config{Profile: p, Store: st, Connect: connect, Worker: workerService, Tasks: tasks, AdapterVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	out, appErr := svc.Await(context.Background(), contract.AwaitInput{SubmissionID: subID})
	if appErr != nil {
		t.Fatalf("retained await: %s", appErr.Message)
	}
	if upstreamCalls != 0 {
		t.Fatalf("retained await called upstream %d times", upstreamCalls)
	}
	return out
}

var errUpstreamCalled = errString("upstream unavailable")

type errString string

func (e errString) Error() string { return string(e) }

func processEnv(db, key, endpoint, pidPath, subPath, mode, binding string) []string {
	return append(os.Environ(),
		"APP_PROCTEST_DB="+db,
		"APP_PROCTEST_KEY="+key,
		"APP_PROCTEST_URL="+endpoint,
		"APP_PROCTEST_PIDFILE="+pidPath,
		"APP_PROCTEST_SUBFILE="+subPath,
		"APP_PROCTEST_MODE="+mode,
		"APP_PROCTEST_BINDING="+binding,
	)
}

func readTrim(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(trimSpace(raw))
}

func trimSpace(raw []byte) []byte {
	for len(raw) > 0 && (raw[len(raw)-1] == '\n' || raw[len(raw)-1] == ' ') {
		raw = raw[:len(raw)-1]
	}
	return raw
}

func runProcess(t *testing.T, env []string, phase string) (int, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, scenarioArgPrefix+"task-process")
	cmd.Env = append(append([]string{}, env...), "APP_PROCTEST_PHASE="+phase)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatal(err)
	}
	return exit.ExitCode(), string(out)
}

func readPIDs(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pids []string
	for _, line := range splitLines(string(raw)) {
		if line != "" {
			pids = append(pids, line)
		}
	}
	return pids
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func taskProcessScenario() int {
	dbPath := os.Getenv("APP_PROCTEST_DB")
	keyPath := os.Getenv("APP_PROCTEST_KEY")
	endpoint := os.Getenv("APP_PROCTEST_URL")
	phase := os.Getenv("APP_PROCTEST_PHASE")
	mode := os.Getenv("APP_PROCTEST_MODE")
	binding := os.Getenv("APP_PROCTEST_BINDING")
	st, err := store.Open(context.Background(), dbPath, appFileKeys{path: keyPath}, store.Config{Limits: limits.Default()})
	if err != nil {
		return 2
	}
	defer func() { _ = st.Close() }()
	p, err := newFixtureProfile(endpoint)
	if err != nil {
		return 3
	}
	if binding == "unproven" {
		stripReplayBinding(p)
	}
	svc, tasks, workerService, err := scenarioService(st, p, endpoint)
	if err != nil {
		return 4
	}
	defer tasks.Stop()
	defer workerService.Stop()
	_ = tasks.Start(context.Background())
	switch phase {
	case "a":
		return scenarioPhaseA(st, svc, mode, binding)
	case "b":
		return scenarioPhaseB(st, tasks, mode)
	default:
		return 5
	}
}

func scenarioPhaseA(st *store.Store, svc *Service, mode, binding string) int {
	out, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "message",
		ClientRequestID: "proc-1",
		Arguments:       json.RawMessage(`{"message":"hello"}`),
	})
	if appErr != nil {
		return 6
	}
	if err := os.WriteFile(os.Getenv("APP_PROCTEST_SUBFILE"), []byte(out.SubmissionID), 0o600); err != nil {
		return 12
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		sub, err := st.GetSubmission(context.Background(), out.SubmissionID)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if mode == "ambiguous" && binding == "unproven" && sub.Status == contract.StatusOutcomeUnknown {
			return writePID()
		}
		if mode == "restart" && sub.TaskID == "task-1" && !terminalStatus(sub.Status) {
			return writePID()
		}
		if mode == "ambiguous" && binding == "proven" && sub.Status == contract.StatusRunning && sub.TaskID == "" {
			time.Sleep(100 * time.Millisecond)
			again, err := st.GetSubmission(context.Background(), out.SubmissionID)
			if err == nil && again.TaskID == "" && again.Status == contract.StatusRunning {
				return writePID()
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 7
}

func scenarioPhaseB(st *store.Store, tasks *TaskService, mode string) int {
	raw, err := os.ReadFile(os.Getenv("APP_PROCTEST_SUBFILE"))
	if err != nil {
		return 13
	}
	id := string(trimSpace(raw))
	_ = tasks.Start(context.Background())
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		sub, err := st.GetSubmission(context.Background(), id)
		if err != nil {
			return 8
		}
		if mode == "ambiguous" && sub.Status == contract.StatusOutcomeUnknown {
			return writePID()
		}
		if sub.Status == contract.StatusCompleted && sub.TaskID == "task-1" {
			return writePID()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 9
}

func scenarioService(st *store.Store, p *profile.Profile, endpoint string) (*Service, *TaskService, *worker.Service, error) {
	up, err := upstream.New(upstream.Config{
		Endpoint:           endpoint,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "test"},
		ClientCapabilities: json.RawMessage(`{"extensions":{"io.modelcontextprotocol/tasks":{}}}`),
		TokenProvider:      func(context.Context) (string, error) { return "test-token", nil },
		MaxResponseBytes:   int64(limits.Default().ResponseBytes),
		HTTPClient:         &http.Client{},
	})
	if err != nil {
		return nil, nil, nil, err
	}
	adapter, err := tama2026.New(tama2026.Config{Profile: *p, Upstream: up, AdapterVersion: "test"})
	if err != nil {
		return nil, nil, nil, err
	}
	connect := MemoConnect(func(ctx context.Context) (*tama2026.Connection, error) { return adapter.Connect(ctx) })
	workerService, err := worker.NewService(st, NewExecutor(connect), worker.Config{
		Owner:    "proc-" + strconv.Itoa(os.Getpid()),
		LeaseTTL: 30 * time.Second,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	tasks, err := NewTaskService(st, connect, TaskConfig{
		Owner:         "proc-tasks-" + strconv.Itoa(os.Getpid()),
		LeaseTTL:      30 * time.Second,
		SweepInterval: time.Hour,
	})
	if err != nil {
		workerService.Stop()
		return nil, nil, nil, err
	}
	svc, err := New(Config{
		Profile: p, Store: st, Connect: connect, Worker: workerService, Tasks: tasks, AdapterVersion: "test",
	})
	if err != nil {
		tasks.Stop()
		workerService.Stop()
		return nil, nil, nil, err
	}
	return svc, tasks, workerService, nil
}

func stripReplayBinding(p *profile.Profile) {
	for i := range p.Operations {
		if p.Operations[i].Name != "message" {
			continue
		}
		p.Operations[i].Bindings = nil
		digest, err := p.Operations[i].ComputeDigest()
		if err != nil {
			return
		}
		p.Operations[i].Digest = digest
	}
}

func terminalStatus(status contract.Status) bool {
	switch status {
	case contract.StatusCompleted, contract.StatusFailed, contract.StatusCancelled,
		contract.StatusExpired, contract.StatusOutcomeUnknown:
		return true
	default:
		return false
	}
}

func writePID() int {
	f, err := os.OpenFile(os.Getenv("APP_PROCTEST_PIDFILE"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 10
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		return 11
	}
	return 0
}
