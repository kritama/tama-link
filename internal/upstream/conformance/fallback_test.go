package conformance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kritama/tama-link/internal/upstream"
)

// TestPollingFallbackAfterSubscriptionRefusal proves a method-not-found
// subscription does not block recovery through tasks/get. Notifications are
// optional; this is not live acceptance.
func TestPollingFallbackAfterSubscriptionRefusal(t *testing.T) {
	set := mustLoad(t)
	completed := findFixture(t, set.Tasks, "tasks/get completed")
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var rpc struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)
		calls = append(calls, rpc.Method)
		w.Header().Set("Content-Type", "application/json")
		if rpc.Method == upstream.MethodSubscribe {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + jsonString(rpc.ID) +
				`,"error":{"code":-32601,"message":"Method not found: subscriptions/listen"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rewriteID(completed.Expected.Body, completed.RequestID(), rpc.ID))
	}))
	defer server.Close()
	client := newClient(t, server.URL)
	err := client.Subscribe(context.Background(), []string{"task-get-completed"}, &upstream.SubscribeCallbacks{
		OnAcknowledged: func([]string) error { return nil },
		OnTask: func(upstream.TaskState) error {
			t.Error("polling fallback delivered a notification")
			return nil
		},
	})
	if !upstream.IsProtocol(err) {
		t.Fatalf("subscription refusal: %v", err)
	}
	state, err := client.TaskGet(context.Background(), "task-get-completed")
	if err != nil {
		t.Fatalf("tasks/get recovery: %v", err)
	}
	if state.Status != upstream.TaskCompleted || state.TaskID != "task-get-completed" {
		t.Fatalf("recovered %s/%s", state.TaskID, state.Status)
	}
	if len(calls) != 2 || calls[0] != upstream.MethodSubscribe || calls[1] != upstream.MethodTaskGet {
		t.Fatalf("calls = %v, want subscribe then tasks/get", calls)
	}
}

// TestDroppedSubscriptionReconcilesThroughTaskGet proves an acknowledgement
// followed by a close, with no task snapshot, is recovered by tasks/get
// rather than by replaying tools/call.
func TestDroppedSubscriptionReconcilesThroughTaskGet(t *testing.T) {
	set := mustLoad(t)
	policy := findFixture(t, set.Subscriptions, "subscriptions/listen stale policy closes stream")
	completed := findFixture(t, set.Tasks, "tasks/get completed")
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var rpc struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)
		calls = append(calls, rpc.Method)
		if rpc.Method == upstream.MethodSubscribe {
			writeExpected(w, policy, rpc.ID)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rewriteID(completed.Expected.Body, completed.RequestID(), rpc.ID))
	}))
	defer server.Close()
	client := newClient(t, server.URL)
	var snapshots int
	err := client.Subscribe(context.Background(), []string{"subscription-stale"}, &upstream.SubscribeCallbacks{
		OnAcknowledged: func([]string) error { return nil },
		OnTask: func(upstream.TaskState) error {
			snapshots++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("dropped subscription: %v", err)
	}
	if snapshots != 0 {
		t.Fatalf("dropped stream delivered %d snapshots", snapshots)
	}
	state, err := client.TaskGet(context.Background(), "task-get-completed")
	if err != nil {
		t.Fatalf("tasks/get recovery: %v", err)
	}
	if !state.IsTerminal() {
		t.Fatalf("recovered status %s, want terminal", state.Status)
	}
	for _, method := range calls {
		if method == upstream.MethodCallTool || forbiddenMethod(method) {
			t.Errorf("recovery emitted %s", method)
		}
	}
}

func findFixture(t *testing.T, fixtures []Fixture, name string) Fixture {
	t.Helper()
	for _, fx := range fixtures {
		if fx.Name == name {
			return fx
		}
	}
	t.Fatalf("fixture %q not in pin", name)
	return Fixture{}
}

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
}
