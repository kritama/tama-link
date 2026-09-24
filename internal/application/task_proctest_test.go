package application

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/upstream"
	"github.com/kritama/tama-link/internal/worker"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestInputDeliverySingleWinnerAcrossProcesses(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.onGet = func(int) (int, string) {
		return http.StatusOK, taskState("input_required", "2026-09-11T10:00:04Z", taskInputRequests())
	}
	dir := t.TempDir()
	dbPath := dir + "/state.db"
	keyPath := dir + "/key"
	st, err := store.Open(context.Background(), dbPath, appFileKeys{path: keyPath}, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := newFixtureProfile(up.ts.URL + "/mcp/app")
	if err != nil {
		t.Fatal(err)
	}
	message, _ := profile.Catalog().Find("message")
	created, _, err := st.CreateSubmission(context.Background(), store.NewSubmission{
		ID:               "sub-input-race",
		ClientRequestID:  "proc-1",
		Tool:             "message",
		Strategy:         "upstream_task",
		DescriptorDigest: message.Digest,
		Arguments:        json.RawMessage(`{"message":"hello","identifier":"proc-1"}`),
		RequestArguments: json.RawMessage(`{"message":"hello"}`),
		ProtocolVersion:  "2026-07-28",
		AdapterVersion:   "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []contract.Status{contract.StatusQueued, contract.StatusRunning, contract.StatusInputRequired} {
		if _, err := st.Transition(context.Background(), created.ID, status, store.TransitionDetail{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AttachTaskID(context.Background(), created.ID, "task-1"); err != nil {
		t.Fatal(err)
	}
	owner := "seed"
	if ok, err := st.ClaimLease(context.Background(), "submission/"+created.ID, owner, time.Minute); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := st.SaveTaskRecordLeased(context.Background(), created.ID, "submission/"+created.ID, owner, store.TaskRecord{
		InputRequests: []byte(taskInputRequestsObject()),
		UpdatedAt:     "2026-09-11T10:00:04Z",
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.ReleaseLease(context.Background(), "submission/"+created.ID, owner)
	_ = st.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(),
		"APP_PROCTEST_DB="+dbPath,
		"APP_PROCTEST_KEY="+keyPath,
		"APP_PROCTEST_URL="+up.ts.URL+"/mcp/app",
		"APP_PROCTEST_SUB="+created.ID,
	)
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(exe, scenarioArgPrefix+"input-delivery")
			cmd.Env = env
			out, runErr := cmd.CombinedOutput()
			if runErr != nil {
				errCh <- fmtError(runErr, out)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if up.updateCount() != 1 {
		t.Fatalf("cross-process updates = %d, want 1", up.updateCount())
	}
}

func inputDeliveryScenario() int {
	dbPath := os.Getenv("APP_PROCTEST_DB")
	keyPath := os.Getenv("APP_PROCTEST_KEY")
	endpoint := os.Getenv("APP_PROCTEST_URL")
	subID := os.Getenv("APP_PROCTEST_SUB")
	st, err := store.Open(context.Background(), dbPath, appFileKeys{path: keyPath}, store.Config{Limits: limits.Default()})
	if err != nil {
		return 2
	}
	defer func() { _ = st.Close() }()
	p, err := newFixtureProfile(endpoint)
	if err != nil {
		return 3
	}
	up, err := upstream.New(upstream.Config{
		Endpoint:           endpoint,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "test"},
		ClientCapabilities: json.RawMessage(`{"extensions":{"io.modelcontextprotocol/tasks":{}}}`),
		TokenProvider:      func(context.Context) (string, error) { return "test-token", nil },
		MaxResponseBytes:   int64(limits.Default().ResponseBytes),
		HTTPClient:         &http.Client{},
	})
	if err != nil {
		return 4
	}
	adapter, err := tama2026.New(tama2026.Config{Profile: *p, Upstream: up, AdapterVersion: "test"})
	if err != nil {
		return 5
	}
	connect := MemoConnect(func(ctx context.Context) (*tama2026.Connection, error) { return adapter.Connect(ctx) })
	workerService, err := worker.NewService(st, NewExecutor(connect), worker.Config{Owner: "proc", LeaseTTL: 30 * time.Second})
	if err != nil {
		return 6
	}
	defer workerService.Stop()
	tasks, err := NewTaskService(st, connect, TaskConfig{Owner: "proc-tasks", LeaseTTL: 30 * time.Second})
	if err != nil {
		return 7
	}
	defer tasks.Stop()
	svc, err := New(Config{Profile: p, Store: st, Connect: connect, Worker: workerService, Tasks: tasks, AdapterVersion: "test"})
	if err != nil {
		return 8
	}
	_, appErr := svc.Await(context.Background(), contract.AwaitInput{
		SubmissionID:   subID,
		TimeoutMS:      1,
		InputResponses: map[string]json.RawMessage{"approval": json.RawMessage(`{"action":"accept"}`)},
	})
	if appErr != nil {
		return 9
	}
	return 0
}

func taskInputRequestsObject() string {
	return `{"approval":{"mode":"elicitation","schema":{"type":"object"}}}`
}

func fmtError(err error, out []byte) error {
	return &scenarioError{err: err, out: out}
}

type scenarioError struct {
	err error
	out []byte
}

func (e *scenarioError) Error() string {
	return e.err.Error() + "\n" + string(e.out)
}
