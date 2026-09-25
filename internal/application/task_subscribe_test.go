package application

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
)

func TestNextWatchDelayBacksOffUntilThePollCap(t *testing.T) {
	t.Parallel()

	capDelay := 400 * time.Millisecond
	delay := nextWatchDelay(taskPollFloor, false, capDelay)
	if delay != 40*time.Millisecond {
		t.Fatalf("first backoff = %s", delay)
	}
	delay = nextWatchDelay(delay, false, capDelay)
	if delay != 80*time.Millisecond {
		t.Fatalf("second backoff = %s", delay)
	}
	if got := nextWatchDelay(300*time.Millisecond, false, capDelay); got != capDelay {
		t.Fatalf("capped backoff = %s", got)
	}
	if got := nextWatchDelay(capDelay, true, capDelay); got != taskPollFloor {
		t.Fatalf("reset = %s", got)
	}
}

func TestTaskSubscriptionSnapshotsAndFallback(t *testing.T) {
	t.Parallel()

	t.Run("polling fallback", func(t *testing.T) {
		t.Parallel()
		up := newTaskUpstream(t)
		up.onGet = func(int) (int, string) {
			if up.subscribeCount() == 0 {
				return http.StatusOK, taskState("working", "2026-09-11T10:00:02Z", "")
			}
			return http.StatusOK, taskState("completed", "2026-09-11T10:00:08Z", taskSuccessResult(false))
		}
		svc, st := taskApp(t, up)
		out := submitMessage(t, svc, "poll")
		waitStatus(t, st, out, contract.StatusCompleted)
		if up.subscribeCount() == 0 {
			t.Fatal("polling fallback never attempted a subscription")
		}
	})

	t.Run("complete snapshot", func(t *testing.T) {
		t.Parallel()
		up := newTaskUpstream(t)
		up.onGet = func(int) (int, string) {
			return http.StatusOK, taskState("working", "2026-09-11T10:00:02Z", "")
		}
		up.onSubscribe = func(w http.ResponseWriter, _ *http.Request, id string) {
			writeSSE(w, ackSSE(id, "task-1"), taskNote(id, "completed", "2026-09-11T10:00:06Z", taskSuccessResult(false)), finalSSE(id))
		}
		svc, st := taskApp(t, up)
		out := submitMessage(t, svc, "snap")
		sub := waitStatus(t, st, out, contract.StatusCompleted)
		if sub.Result == nil || sub.Result.IsError {
			t.Fatalf("snapshot result = %+v", sub.Result)
		}
		if !strings.Contains(sub.TaskCapabilities, "io.modelcontextprotocol/tasks") {
			t.Fatalf("subscription snapshot erased capabilities: %q", sub.TaskCapabilities)
		}
	})

	t.Run("acknowledgement first", func(t *testing.T) {
		t.Parallel()
		up := newTaskUpstream(t)
		up.onGet = func(int) (int, string) {
			return http.StatusOK, taskState("working", "2026-09-11T10:00:02Z", "")
		}
		up.onSubscribe = func(w http.ResponseWriter, _ *http.Request, id string) {
			writeSSE(w, taskNote(id, "completed", "2026-09-11T10:00:06Z", taskSuccessResult(false)))
			block(w)
		}
		svc, st := taskApp(t, up)
		out := submitMessage(t, svc, "ack")
		waitUntil(t, func() bool { return up.subscribeCount() > 0 })
		time.Sleep(80 * time.Millisecond)
		sub, err := st.GetSubmission(t.Context(), out)
		if err != nil {
			t.Fatal(err)
		}
		if sub.Status == contract.StatusCompleted {
			t.Fatal("pre-acknowledgement snapshot was applied")
		}
	})

	t.Run("authorized subset", func(t *testing.T) {
		t.Parallel()
		up := newTaskUpstream(t)
		// Opening subscriptions/listen is not the fallback. A tasks/get can
		// complete the task only after the empty acknowledgement is sent, so
		// removing the later poll cannot turn this green.
		var acked atomic.Bool
		var getsAfterAck atomic.Int32
		up.onGet = func(int) (int, string) {
			if !acked.Load() {
				return http.StatusOK, taskState("working", "2026-09-11T10:00:02Z", "")
			}
			getsAfterAck.Add(1)
			return http.StatusOK, taskState("completed", "2026-09-11T10:00:08Z", taskSuccessResult(false))
		}
		up.onSubscribe = func(w http.ResponseWriter, _ *http.Request, id string) {
			// An empty authorized set is a valid subset: this task was not authorized.
			writeSSE(w, ackSSE(id))
			acked.Store(true)
			block(w)
		}
		svc, st := taskApp(t, up)
		out := submitMessage(t, svc, "subset")
		waitStatus(t, st, out, contract.StatusCompleted)
		if up.subscribeCount() == 0 {
			t.Fatal("authorized-subset fallback never subscribed")
		}
		if getsAfterAck.Load() == 0 {
			t.Fatal("authorized-subset fallback did not poll tasks/get after the empty acknowledgement")
		}
	})

	t.Run("reconnect reconciles through tasks/get", func(t *testing.T) {
		t.Parallel()
		up := newTaskUpstream(t)
		up.onGet = func(n int) (int, string) {
			if n < 2 {
				return http.StatusOK, taskState("working", "2026-09-11T10:00:02Z", "")
			}
			return http.StatusOK, taskState("completed", "2026-09-11T10:00:07Z", taskSuccessResult(false))
		}
		up.onSubscribe = func(w http.ResponseWriter, _ *http.Request, id string) {
			writeSSE(w, ackSSE(id, "task-1"))
		}
		svc, st := taskApp(t, up)
		out := submitMessage(t, svc, "reconnect")
		waitStatus(t, st, out, contract.StatusCompleted)
		if up.subscribeCount() == 0 {
			t.Fatal("reconnect path did not subscribe")
		}
	})

	t.Run("credential expiry closes an open stream", func(t *testing.T) {
		t.Parallel()
		up := newTaskUpstream(t)
		started := make(chan struct{})
		var subs atomic.Int32
		up.onGet = func(int) (int, string) {
			if subs.Load() < 2 {
				return http.StatusOK, taskState("working", "2026-09-11T10:00:02Z", "")
			}
			return http.StatusOK, taskState("completed", "2026-09-11T10:00:08Z", taskSuccessResult(false))
		}
		up.onSubscribe = func(w http.ResponseWriter, r *http.Request, id string) {
			n := subs.Add(1)
			writeSSE(w, ackSSE(id, "task-1"))
			if n == 1 {
				close(started)
				<-r.Context().Done()
			}
		}
		fire := make(chan time.Time, 1)
		session := &fakeSession{expiry: time.Now().Add(time.Hour)}
		cfg := appFixtureConfigTuned(t, &fakeTama{ts: up.ts}, limits.Default(), nil, func(taskCfg *TaskConfig) {
			taskCfg.Credentials = session
			taskCfg.Now = time.Now
			taskCfg.After = func(time.Duration) <-chan time.Time { return fire }
		})
		svc, st, _ := appFromConfig(t, cfg)
		out := submitMessage(t, svc, "expiry-stream")
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("subscription stream did not open")
		}
		fire <- time.Now()
		waitStatus(t, st, out, contract.StatusCompleted)
		if session.refreshes() == 0 {
			t.Fatal("credential expiry did not refresh before resubscribe")
		}
		if up.subscribeCount() < 2 {
			t.Fatalf("subscribes = %d, want a resubscribe after expiry", up.subscribeCount())
		}
	})

	t.Run("equal timestamp status change is applied", func(t *testing.T) {
		t.Parallel()
		up := newTaskUpstream(t)
		var advanced atomic.Bool
		up.onGet = func(int) (int, string) {
			if !advanced.Load() {
				return http.StatusOK, taskState("input_required", "2026-09-11T10:00:05Z", taskInputRequests())
			}
			return http.StatusOK, taskState("working", "2026-09-11T10:00:05Z", "")
		}
		svc, st := taskApp(t, up)
		out := submitMessage(t, svc, "same-stamp")
		waitStatus(t, st, out, contract.StatusInputRequired)
		advanced.Store(true)
		waitStatus(t, st, out, contract.StatusRunning)
	})

	t.Run("dropped subscription backs off", func(t *testing.T) {
		t.Parallel()
		up := newTaskUpstream(t)
		up.pollIntervalMs = 400
		up.onGet = func(int) (int, string) {
			return http.StatusOK, strings.Replace(taskState("working", "2026-09-11T10:00:02Z", ""), `"pollIntervalMs":20`, `"pollIntervalMs":400`, 1)
		}
		up.onSubscribe = func(w http.ResponseWriter, _ *http.Request, id string) {
			writeSSE(w, ackSSE(id, "task-1"))
		}
		svc, _ := taskApp(t, up)
		_ = submitMessage(t, svc, "backoff")
		waitUntil(t, func() bool { return up.subscribeCount() > 0 })
		time.Sleep(250 * time.Millisecond)
		if up.subscribeCount() > 5 {
			t.Fatalf("subscribes = %d, want backoff instead of a 20ms reconnect storm", up.subscribeCount())
		}
		svc.tasks.Stop()
	})

	t.Run("duplicate snapshot does not regress", func(t *testing.T) {
		t.Parallel()
		up := newTaskUpstream(t)
		up.onGet = func(int) (int, string) {
			return http.StatusOK, taskState("input_required", "2026-09-11T10:00:05Z", taskInputRequests())
		}
		up.onSubscribe = func(w http.ResponseWriter, _ *http.Request, id string) {
			writeSSE(w, ackSSE(id, "task-1"), taskNote(id, "working", "2026-09-11T10:00:02Z", ""))
			block(w)
		}
		svc, st := taskApp(t, up)
		out := submitMessage(t, svc, "order")
		waitStatus(t, st, out, contract.StatusInputRequired)
		time.Sleep(80 * time.Millisecond)
		sub, err := st.GetSubmission(t.Context(), out)
		if err != nil {
			t.Fatal(err)
		}
		if sub.Status != contract.StatusInputRequired {
			t.Fatalf("older snapshot regressed state to %s", sub.Status)
		}
	})
}

func submitMessage(t *testing.T, svc *Service, id string) string {
	t.Helper()
	out, appErr := svc.Submit(t.Context(), messageInput(id))
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	return out.SubmissionID
}

func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, event := range events {
		_, _ = fmt.Fprint(w, event)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func block(w http.ResponseWriter) {
	if controller := http.NewResponseController(w); controller != nil {
		_ = controller.SetWriteDeadline(time.Now().Add(2 * time.Second))
	}
	time.Sleep(150 * time.Millisecond)
}

func ackSSE(id string, taskIDs ...string) string {
	ids := ""
	for i, taskID := range taskIDs {
		if i > 0 {
			ids += ","
		}
		ids += fmt.Sprintf("%q", taskID)
	}
	return sse(fmt.Sprintf(
		`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%q},"notifications":{"taskIds":[%s]}}}`,
		id, ids))
}

type fakeSession struct {
	mu       sync.Mutex
	expiry   time.Time
	nRefresh int
}

func (f *fakeSession) Expiry() (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.expiry, !f.expiry.IsZero()
}

func (f *fakeSession) Refresh(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nRefresh++
	f.expiry = time.Now().Add(time.Hour)
	return "refreshed-token", nil
}

func (f *fakeSession) refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nRefresh
}

func taskNote(id, status, updated, extra string) string {
	payload := fmt.Sprintf(
		`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%q},"taskId":"task-1","status":%q,"createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":%q,"ttlMs":60000,"pollIntervalMs":20`,
		id, status, updated)
	if extra != "" {
		payload += "," + extra
	}
	return sse(payload + "}}")
}

func finalSSE(id string) string {
	return sse(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%q,"result":{"resultType":"complete","_meta":{"io.modelcontextprotocol/subscriptionId":%q}}}`,
		id, id))
}

func sse(payload string) string {
	return "data: " + payload + "\n\n"
}
