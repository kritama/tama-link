package application

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/store"
)

func TestTaskSubmitAcceptsBeforeUpstreamReturns(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.holdCall = make(chan struct{})
	svc, st := taskApp(t, up)

	out, appErr := svc.Submit(context.Background(), messageInput("hold-1"))
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	sub, err := st.GetSubmission(context.Background(), out.SubmissionID)
	if err != nil {
		t.Fatalf("accepted row missing while tools/call is blocked: %v", err)
	}
	if sub.ClientRequestID != "hold-1" {
		t.Fatalf("client request id = %s", sub.ClientRequestID)
	}
	close(up.holdCall)
	waitStatus(t, st, out.SubmissionID, contract.StatusCompleted)
}

func TestTaskSubmitIsIdempotentAndConflictSafe(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	svc, _ := taskApp(t, up)

	first, appErr := svc.Submit(context.Background(), messageInput("same"))
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	second, appErr := svc.Submit(context.Background(), messageInput("same"))
	if appErr != nil {
		t.Fatalf("retry: %s", appErr.Message)
	}
	if second.SubmissionID != first.SubmissionID {
		t.Fatalf("retry id = %s, want %s", second.SubmissionID, first.SubmissionID)
	}
	changed := messageInput("same")
	changed.Arguments = json.RawMessage(`{"message":"other"}`)
	if _, appErr = svc.Submit(context.Background(), changed); appErr == nil || appErr.Code != contract.CodeIdempotencyConflict {
		t.Fatalf("conflict = %+v", appErr)
	}
	waitUntil(t, func() bool { return up.callCount() >= 1 })
	if got := up.callCount(); got != 1 {
		t.Fatalf("tools/call count = %d, want 1", got)
	}
}

func TestTaskCallIsServerDirected(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	svc, st := taskApp(t, up)
	out, appErr := svc.Submit(context.Background(), messageInput("directed"))
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	waitStatus(t, st, out.SubmissionID, contract.StatusCompleted)

	var envelope struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(up.callBody()), &envelope); err != nil {
		t.Fatalf("decode call: %v", err)
	}
	if envelope.Method != "tools/call" {
		t.Fatalf("method = %s", envelope.Method)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Params, &params); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if _, ok := params["task"]; ok {
		t.Fatal("tools/call carried params.task")
	}
	if !strings.Contains(string(params["_meta"]), "io.modelcontextprotocol/tasks") {
		t.Fatalf("tools/call did not declare the tasks capability: %s", params["_meta"])
	}
	if !strings.Contains(string(params["arguments"]), `"identifier":"directed"`) {
		t.Fatalf("bindings were not applied: %s", params["arguments"])
	}
	sub, err := st.GetSubmission(context.Background(), out.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.TaskID != "task-1" {
		t.Fatalf("task id = %q", sub.TaskID)
	}
	if sub.TaskPollIntervalMs != 20 || sub.TaskTTLMs != 60000 {
		t.Fatalf("ttl %d poll %d", sub.TaskTTLMs, sub.TaskPollIntervalMs)
	}
	if !strings.Contains(sub.TaskCapabilities, "io.modelcontextprotocol/tasks") {
		t.Fatalf("capability snapshot = %s", sub.TaskCapabilities)
	}
}

func TestTaskStatesAndTerminalCapture(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		state  string
		extra  string
		status contract.Status
		code   contract.Code
	}{
		{name: "completed", state: "completed", extra: taskSuccessResult(false), status: contract.StatusCompleted},
		{name: "isError", state: "completed", extra: taskSuccessResult(true), status: contract.StatusCompleted},
		{name: "failed", state: "failed", extra: `"error":{"code":-32603,"message":"The message failed."}`, status: contract.StatusFailed, code: contract.CodeUpstreamExecutionFailed},
		{name: "cancelled", state: "cancelled", status: contract.StatusCancelled, code: contract.CodeUpstreamExecutionFailed},
		{name: "expired", state: "failed", extra: `"error":{"code":-32603,"message":"Task TTL expired"}`, status: contract.StatusExpired, code: contract.CodeUpstreamExecutionFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			up := newTaskUpstream(t)
			up.onGet = func(int) (int, string) {
				return http.StatusOK, taskState(tc.state, "2026-09-11T10:00:05Z", tc.extra)
			}
			svc, st := taskApp(t, up)
			out, appErr := svc.Submit(context.Background(), messageInput(tc.name))
			if appErr != nil {
				t.Fatalf("submit: %s", appErr.Message)
			}
			sub := waitStatus(t, st, out.SubmissionID, tc.status)
			if tc.status == contract.StatusCompleted {
				if sub.Result == nil || sub.Result.IsError != (tc.name == "isError") {
					t.Fatalf("result = %+v", sub.Result)
				}
				return
			}
			if sub.ErrorCode != string(tc.code) {
				t.Fatalf("error = %s %s", sub.ErrorCode, sub.ErrorMessage)
			}
			if tc.status == contract.StatusExpired && strings.Contains(strings.ToLower(sub.ErrorMessage), "not found") {
				t.Fatalf("expiry message distinguishes lookup failure: %s", sub.ErrorMessage)
			}
		})
	}
}

func TestTaskUnavailableLookupsAreIndistinguishable(t *testing.T) {
	t.Parallel()

	messages := []string{"Task not found", "Task was not found"}
	var got []string
	for _, message := range messages {
		up := newTaskUpstream(t)
		up.onGet = func(int) (int, string) {
			return http.StatusBadRequest, `{"code":-32602,"message":` + jsonQuote(message) + `}`
		}
		svc, st := taskApp(t, up)
		out, appErr := svc.Submit(context.Background(), messageInput(message))
		if appErr != nil {
			t.Fatalf("submit: %s", appErr.Message)
		}
		sub := waitStatus(t, st, out.SubmissionID, contract.StatusFailed)
		got = append(got, sub.ErrorCode+"|"+sub.ErrorMessage)
	}
	if got[0] != got[1] {
		t.Fatalf("lookup errors diverged: %q vs %q", got[0], got[1])
	}
	if strings.Contains(strings.ToLower(got[0]), "not found") || strings.Contains(strings.ToLower(got[0]), "unauthorized") {
		t.Fatalf("safe error leaked the lookup cause: %s", got[0])
	}
}

func TestTaskInputResponses(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	updated := make(chan struct{}, 1)
	up.onGet = func(int) (int, string) {
		if up.updateCount() == 0 {
			return http.StatusOK, taskState("input_required", "2026-09-11T10:00:04Z", taskInputRequests())
		}
		return http.StatusOK, taskState("completed", "2026-09-11T10:00:06Z", taskSuccessResult(false))
	}
	svc, st := taskApp(t, up)
	out, appErr := svc.Submit(context.Background(), messageInput("input"))
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	pending := waitStatus(t, st, out.SubmissionID, contract.StatusInputRequired)
	awaited, appErr := svc.Await(context.Background(), contract.AwaitInput{
		SubmissionID: out.SubmissionID,
		TimeoutMS:    1,
	})
	if appErr != nil {
		t.Fatalf("await: %s", appErr.Message)
	}
	if awaited.Status != contract.StatusInputRequired || !strings.Contains(string(awaited.InputRequests), `"approval"`) {
		t.Fatalf("await = %+v", awaited)
	}
	if _, appErr = svc.Await(context.Background(), contract.AwaitInput{
		SubmissionID:   out.SubmissionID,
		InputResponses: map[string]json.RawMessage{"stale": json.RawMessage(`{"action":"accept"}`)},
	}); appErr == nil || appErr.Code != contract.CodeInvalidRequest {
		t.Fatalf("stale = %+v", appErr)
	}
	response := json.RawMessage(`{"action":"accept"}`)
	if _, appErr = svc.Await(context.Background(), contract.AwaitInput{
		SubmissionID:   out.SubmissionID,
		InputResponses: map[string]json.RawMessage{"approval": response},
		TimeoutMS:      1,
	}); appErr != nil {
		t.Fatalf("update: %s", appErr.Message)
	}
	select {
	case <-updated:
	default:
	}
	if up.updateCount() != 1 {
		t.Fatalf("updates = %d, want 1", up.updateCount())
	}
	if _, appErr = svc.Await(context.Background(), contract.AwaitInput{
		SubmissionID:   out.SubmissionID,
		InputResponses: map[string]json.RawMessage{"approval": json.RawMessage(`{"action":"reject"}`)},
	}); appErr == nil || appErr.Code != contract.CodeIdempotencyConflict {
		t.Fatalf("conflict = %+v", appErr)
	}
	if _, appErr = svc.Await(context.Background(), contract.AwaitInput{
		SubmissionID:   out.SubmissionID,
		InputResponses: map[string]json.RawMessage{"approval": response},
		TimeoutMS:      1,
	}); appErr != nil {
		t.Fatalf("exact replay: %s", appErr.Message)
	}
	if up.updateCount() != 1 {
		t.Fatalf("exact replay sent another update: %d", up.updateCount())
	}
	_ = pending
	waitStatus(t, st, out.SubmissionID, contract.StatusCompleted)
}

func TestTaskPartialInputResponse(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.onGet = func(int) (int, string) {
		extra := `"inputRequests":{"approval":{"mode":"elicitation"},"note":{"mode":"elicitation"}}`
		return http.StatusOK, taskState("input_required", "2026-09-11T10:00:04Z", extra)
	}
	svc, st := taskApp(t, up)
	out, appErr := svc.Submit(context.Background(), messageInput("partial"))
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	waitStatus(t, st, out.SubmissionID, contract.StatusInputRequired)
	if _, appErr = svc.Await(context.Background(), contract.AwaitInput{
		SubmissionID:   out.SubmissionID,
		TimeoutMS:      1,
		InputResponses: map[string]json.RawMessage{"approval": json.RawMessage(`{"action":"accept"}`)},
	}); appErr != nil {
		t.Fatalf("partial update: %s", appErr.Message)
	}
	if up.updateCount() != 1 || !strings.Contains(up.updateBody(), `"approval"`) || strings.Contains(up.updateBody(), `"note"`) {
		t.Fatalf("partial update body = %s", up.updateBody())
	}
	sub, err := st.GetSubmission(context.Background(), out.SubmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != contract.StatusInputRequired {
		t.Fatalf("partial response left state %s", sub.Status)
	}
}

func TestTaskConcurrentInputDelivery(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.onGet = func(int) (int, string) {
		return http.StatusOK, taskState("input_required", "2026-09-11T10:00:04Z", taskInputRequests())
	}
	svc, st := taskApp(t, up)
	out, appErr := svc.Submit(context.Background(), messageInput("race"))
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	waitStatus(t, st, out.SubmissionID, contract.StatusInputRequired)
	svc.tasks.Stop()

	same := json.RawMessage(`{"action":"accept"}`)
	var wg sync.WaitGroup
	errs := make(chan *contract.Error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Await(context.Background(), contract.AwaitInput{
				SubmissionID:   out.SubmissionID,
				TimeoutMS:      1,
				InputResponses: map[string]json.RawMessage{"approval": same},
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("same-value wait: %s", err.Message)
		}
	}
	if up.updateCount() != 1 {
		t.Fatalf("same-value updates = %d, want 1", up.updateCount())
	}

	conflictUp := newTaskUpstream(t)
	conflictUp.onGet = up.onGet
	conflictSvc, conflictStore := taskApp(t, conflictUp)
	conflictOut, appErr := conflictSvc.Submit(context.Background(), messageInput("conflict-race"))
	if appErr != nil {
		t.Fatal(appErr.Message)
	}
	waitStatus(t, conflictStore, conflictOut.SubmissionID, contract.StatusInputRequired)
	conflictSvc.tasks.Stop()
	errs = make(chan *contract.Error, 2)
	responses := []json.RawMessage{json.RawMessage(`{"action":"accept"}`), json.RawMessage(`{"action":"reject"}`)}
	var conflictWG sync.WaitGroup
	for _, response := range responses {
		conflictWG.Add(1)
		go func(response json.RawMessage) {
			defer conflictWG.Done()
			_, err := conflictSvc.Await(context.Background(), contract.AwaitInput{
				SubmissionID:   conflictOut.SubmissionID,
				TimeoutMS:      1,
				InputResponses: map[string]json.RawMessage{"approval": response},
			})
			errs <- err
		}(response)
	}
	conflictWG.Wait()
	close(errs)
	conflicts := 0
	for err := range errs {
		if err == nil {
			continue
		}
		if err.Code != contract.CodeIdempotencyConflict {
			t.Fatalf("different-value wait: %+v", err)
		}
		conflicts++
	}
	if conflicts != 1 || conflictUp.updateCount() != 1 {
		t.Fatalf("conflicts %d updates %d", conflicts, conflictUp.updateCount())
	}
}

func TestTaskMixedAndLostInputDelivery(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.failUpdate.Store(true)
	up.onGet = func(int) (int, string) {
		extra := `"inputRequests":{"approval":{"mode":"elicitation"},"note":{"mode":"elicitation"}}`
		return http.StatusOK, taskState("input_required", "2026-09-11T10:00:04Z", extra)
	}
	svc, st := taskApp(t, up)
	out, appErr := svc.Submit(context.Background(), messageInput("mixed"))
	if appErr != nil {
		t.Fatal(appErr.Message)
	}
	waitStatus(t, st, out.SubmissionID, contract.StatusInputRequired)
	svc.tasks.Stop()
	if err := st.SetInputResponse(context.Background(), out.SubmissionID, "note", json.RawMessage(`{"text":"kept"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkInputResponsesSent(context.Background(), out.SubmissionID, []string{"note"}); err != nil {
		t.Fatal(err)
	}

	if _, appErr = svc.Await(context.Background(), contract.AwaitInput{
		SubmissionID: out.SubmissionID,
		TimeoutMS:    1,
		InputResponses: map[string]json.RawMessage{
			"approval": json.RawMessage(`{"action":"accept"}`),
			"note":     json.RawMessage(`{"text":"kept"}`),
		},
	}); appErr == nil {
		t.Fatal("lost acknowledgement returned success")
	}
	pending, err := st.InputResponsePending(context.Background(), out.SubmissionID, "approval")
	if err != nil || !pending {
		t.Fatalf("lost ack pending = %v %v", pending, err)
	}
	if _, appErr = svc.Await(context.Background(), contract.AwaitInput{
		SubmissionID: out.SubmissionID,
		TimeoutMS:    1,
		InputResponses: map[string]json.RawMessage{
			"approval": json.RawMessage(`{"action":"accept"}`),
			"note":     json.RawMessage(`{"text":"kept"}`),
		},
	}); appErr != nil {
		t.Fatalf("retry: %s", appErr.Message)
	}
	if !strings.Contains(up.updateBody(), `"approval"`) || strings.Contains(up.updateBody(), `"note"`) {
		t.Fatalf("retry sent %s", up.updateBody())
	}
}

func TestTaskAwaitCancellationDoesNotCancelUpstream(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.onGet = func(int) (int, string) {
		return http.StatusOK, taskState("working", "2026-09-11T10:00:03Z", "")
	}
	svc, st := taskApp(t, up)
	out, appErr := svc.Submit(context.Background(), messageInput("wait"))
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	waitUntil(t, func() bool {
		sub, err := st.GetSubmission(context.Background(), out.SubmissionID)
		return err == nil && sub.TaskID != ""
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	awaited, appErr := svc.Await(ctx, contract.AwaitInput{SubmissionID: out.SubmissionID, TimeoutMS: 5000})
	if appErr != nil {
		t.Fatalf("await: %s", appErr.Message)
	}
	if awaited.Terminal {
		t.Fatal("cancelled wait terminated the submission")
	}
	if up.cancelCount() != 0 {
		t.Fatalf("tasks/cancel calls = %d", up.cancelCount())
	}
}

func TestTaskOversizedResponseFailsBeforeDecode(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	limitsCfg := limits.Default()
	limitsCfg.ResponseBytes = 64
	svc, st := taskAppLimits(t, up, limitsCfg)
	out, appErr := svc.Submit(context.Background(), messageInput("wide"))
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	sub := waitStatus(t, st, out.SubmissionID, contract.StatusFailed)
	if sub.ErrorCode != string(contract.CodeResultTooLarge) {
		t.Fatalf("error = %s %s", sub.ErrorCode, sub.ErrorMessage)
	}
	calls := up.callCount()
	time.Sleep(120 * time.Millisecond)
	if up.callCount() != calls {
		t.Fatalf("oversized tools/call was retried: %d then %d", calls, up.callCount())
	}
}

func TestTaskGetOversizedResponseFailsClosed(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.onGet = func(int) (int, string) {
		text := strings.Repeat("x", 2000)
		extra := `"result":{"resultType":"complete","isError":false,"content":[{"type":"text","text":"` + text + `"}],"structuredContent":{"status":"completed"}}`
		return http.StatusOK, taskState("completed", "2026-09-11T10:00:05Z", extra)
	}
	limitsCfg := limits.Default()
	limitsCfg.ResponseBytes = 800
	svc, st := taskAppLimits(t, up, limitsCfg)
	out, appErr := svc.Submit(context.Background(), messageInput("get-wide"))
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	sub := waitStatus(t, st, out.SubmissionID, contract.StatusFailed)
	if sub.ErrorCode != string(contract.CodeResultTooLarge) {
		t.Fatalf("error = %s %s", sub.ErrorCode, sub.ErrorMessage)
	}
	if sub.TaskID == "" {
		t.Fatal("task handle was not stored before the oversized read")
	}
}

func TestTaskStaleInputIsRejectedUnderDeliveryClaim(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.onGet = func(int) (int, string) {
		return http.StatusOK, taskState("input_required", "2026-09-11T10:00:04Z", taskInputRequests())
	}
	svc, st := taskApp(t, up)
	out, appErr := svc.Submit(context.Background(), messageInput("stale-live"))
	if appErr != nil {
		t.Fatal(appErr.Message)
	}
	waitStatus(t, st, out.SubmissionID, contract.StatusInputRequired)
	svc.tasks.Stop()
	if _, err := st.Transition(context.Background(), out.SubmissionID, contract.StatusRunning, store.TransitionDetail{}); err != nil {
		t.Fatal(err)
	}
	owner := "stale-owner"
	if ok, err := st.ClaimLease(context.Background(), "submission/"+out.SubmissionID, owner, time.Minute); err != nil || !ok {
		t.Fatal(err)
	}
	if err := st.SaveTaskRecordLeased(context.Background(), out.SubmissionID, "submission/"+out.SubmissionID, owner, store.TaskRecord{}); err != nil {
		t.Fatal(err)
	}
	_ = st.ReleaseLease(context.Background(), "submission/"+out.SubmissionID, owner)
	if _, appErr = svc.Await(context.Background(), contract.AwaitInput{
		SubmissionID:   out.SubmissionID,
		InputResponses: map[string]json.RawMessage{"approval": json.RawMessage(`{"action":"accept"}`)},
	}); appErr == nil || appErr.Code != contract.CodeInvalidRequest {
		t.Fatalf("stale = %+v", appErr)
	}
	if up.updateCount() != 0 {
		t.Fatalf("stale await sent an update")
	}
}

func TestTaskOversizedResultFailsClosed(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.onGet = func(int) (int, string) {
		text := strings.Repeat("x", 4096)
		extra := `"result":{"resultType":"complete","isError":false,"content":[{"type":"text","text":"` + text + `"}],"structuredContent":{"status":"completed"}}`
		return http.StatusOK, taskState("completed", "2026-09-11T10:00:05Z", extra)
	}
	limitsCfg := limits.Default()
	limitsCfg.ResultBytes = 128
	svc, st := taskAppLimits(t, up, limitsCfg)
	out, appErr := svc.Submit(context.Background(), messageInput("large"))
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	sub := waitStatus(t, st, out.SubmissionID, contract.StatusFailed)
	if sub.ErrorCode != string(contract.CodeResultTooLarge) {
		t.Fatalf("error = %s %s", sub.ErrorCode, sub.ErrorMessage)
	}
}

func TestTaskRestartResumesSameTask(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	done := false
	up.onGet = func(int) (int, string) {
		if !done {
			return http.StatusOK, taskState("working", "2026-09-11T10:00:02Z", "")
		}
		return http.StatusOK, taskState("completed", "2026-09-11T10:00:08Z", taskSuccessResult(false))
	}
	svc, st := taskApp(t, up)
	out, appErr := svc.Submit(context.Background(), messageInput("restart"))
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	waitUntil(t, func() bool {
		sub, err := st.GetSubmission(context.Background(), out.SubmissionID)
		return err == nil && sub.TaskID == "task-1"
	})
	svc.tasks.Stop()
	calls := up.callCount()
	done = true
	recovered, err := NewTaskService(st, svc.connect, TaskConfig{
		Owner:         "recovered",
		LeaseTTL:      30 * time.Second,
		SweepInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(recovered.Stop)
	_ = recovered.Start(context.Background())
	waitStatus(t, st, out.SubmissionID, contract.StatusCompleted)
	if up.callCount() != calls {
		t.Fatalf("restart replayed tools/call: before %d after %d", calls, up.callCount())
	}
	again, appErr := svc.Await(context.Background(), contract.AwaitInput{SubmissionID: out.SubmissionID})
	if appErr != nil || !again.Terminal || again.Result == nil {
		t.Fatalf("retained terminal await = %+v %v", again, appErr)
	}
}

func TestTaskQueuedRowIsAFirstCall(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	svc, st := taskAppWithoutReplayBinding(t, up)
	svc.tasks.Stop()
	id := seedQueuedTask(t, st, svc, "queued-first")
	recovered := startTaskService(t, st, svc.connect)
	_ = recovered.Start(context.Background())
	waitStatus(t, st, id, contract.StatusCompleted)
	if up.callCount() != 1 {
		t.Fatalf("queued recovery calls = %d, want 1", up.callCount())
	}
}

func TestTaskAmbiguousReplayRequiresIdempotencyProof(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	svc, st := taskApp(t, up)
	svc.tasks.Stop()
	proven := seedRunningTask(t, st, svc, "proven")
	recovered := startTaskService(t, st, svc.connect)
	_ = recovered.Start(context.Background())
	waitStatus(t, st, proven, contract.StatusCompleted)
	if up.callCount() != 1 {
		t.Fatalf("proven replay calls = %d, want 1", up.callCount())
	}

	bare := newTaskUpstream(t)
	bareApp, bareStore := taskAppWithoutReplayBinding(t, bare)
	bareApp.tasks.Stop()
	id := seedRunningTask(t, bareStore, bareApp, "unproven")
	bareRunner := startTaskService(t, bareStore, bareApp.connect)
	_ = bareRunner.Start(context.Background())
	sub := waitStatus(t, bareStore, id, contract.StatusOutcomeUnknown)
	if bare.callCount() != 0 {
		t.Fatalf("unproven replay called tools/call %d times", bare.callCount())
	}
	if sub.ErrorCode != string(contract.CodeOutcomeUnknown) {
		t.Fatalf("error = %s", sub.ErrorCode)
	}
}

func messageInput(id string) contract.SubmitInput {
	return contract.SubmitInput{
		Tool:            "message",
		ClientRequestID: id,
		Arguments:       json.RawMessage(`{"message":"hello"}`),
	}
}

func taskApp(t *testing.T, up *taskUpstream) (*Service, *store.Store) {
	t.Helper()
	return taskAppLimits(t, up, limits.Default())
}

func taskAppLimits(t *testing.T, up *taskUpstream, limitsCfg limits.Limits) (*Service, *store.Store) {
	t.Helper()
	cfg := appFixtureConfigWith(t, &fakeTama{ts: up.ts}, limitsCfg, nil)
	svc, st, _ := appFromConfig(t, cfg)
	return svc, st
}

func taskAppWithoutReplayBinding(t *testing.T, up *taskUpstream) (*Service, *store.Store) {
	t.Helper()
	cfg := appFixtureConfigWith(t, &fakeTama{ts: up.ts}, limits.Default(), func(p *profile.Profile) {
		for i := range p.Operations {
			if p.Operations[i].Name != "message" {
				continue
			}
			p.Operations[i].Bindings = nil
			digest, err := p.Operations[i].ComputeDigest()
			if err != nil {
				t.Fatal(err)
			}
			p.Operations[i].Digest = digest
		}
	})
	svc, st, _ := appFromConfig(t, cfg)
	return svc, st
}

func seedRunningTask(t *testing.T, st *store.Store, svc *Service, id string) string {
	t.Helper()
	subID := seedQueuedTask(t, st, svc, id)
	if _, err := st.Transition(context.Background(), subID, contract.StatusRunning, store.TransitionDetail{}); err != nil {
		t.Fatal(err)
	}
	return subID
}

func seedQueuedTask(t *testing.T, st *store.Store, svc *Service, id string) string {
	t.Helper()
	d, ok := svc.profile.Catalog().Find("message")
	if !ok {
		t.Fatal("message descriptor missing")
	}
	sub, _, err := st.CreateSubmission(context.Background(), store.NewSubmission{
		ID:               "sub_" + id,
		ClientRequestID:  id,
		Tool:             d.Name,
		Strategy:         string(d.Strategy),
		DescriptorDigest: d.Digest,
		Arguments:        json.RawMessage(`{"message":"hello","identifier":"` + id + `"}`),
		RequestArguments: json.RawMessage(`{"message":"hello"}`),
		ProtocolVersion:  "2026-07-28",
		AdapterVersion:   "test-adapter",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.Transition(context.Background(), sub.ID, contract.StatusQueued, store.TransitionDetail{}); err != nil {
		t.Fatal(err)
	}
	return sub.ID
}

func startTaskService(t *testing.T, st *store.Store, connect func(context.Context) (*tama2026.Connection, error)) *TaskService {
	t.Helper()
	svc, err := NewTaskService(st, connect, TaskConfig{
		Owner:         "recovered-tasks",
		LeaseTTL:      30 * time.Second,
		SweepInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Stop)
	return svc
}

func waitStatus(t *testing.T, st *store.Store, id string, want contract.Status) *store.Submission {
	t.Helper()
	var last *store.Submission
	waitUntil(t, func() bool {
		sub, err := st.GetSubmission(context.Background(), id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		last = sub
		return sub.Status == want
	})
	return last
}

func waitUntil(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for task condition")
}
