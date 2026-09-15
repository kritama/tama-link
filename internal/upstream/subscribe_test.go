package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const subTaskID = "d59d7f2a-933e-44f4-8c28-4d28e9f0d937"

// newSubClient builds a subscription test client.
func newSubClient(t *testing.T, url string) *Client {
	t.Helper()
	client, err := New(Config{
		Endpoint:           url,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{"extensions":{"io.modelcontextprotocol/tasks":{}}}`),
		TokenProvider:      func(context.Context) (string, error) { return "test-token", nil },
		MaxResponseBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

// subServer streams a scripted sequence of events for a subscriptions/listen
// request, extracting the JSON-RPC request ID from the body.
func subServer(t *testing.T, events func(requestID string) []string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		var envelope struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body[:n], &envelope); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, event := range events(envelope.ID) {
			_, _ = fmt.Fprint(w, event)
			flusher.Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// ackEvent renders the acknowledgement for one subscription stream.
func ackEvent(requestID string, authorized ...string) string {
	ids := ""
	for i, id := range authorized {
		if i > 0 {
			ids += ","
		}
		ids += `"` + id + `"`
	}
	payload := fmt.Sprintf(
		`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%q},"notifications":{"taskIds":[%s]}}}`,
		requestID, ids)
	return sseReply(payload)
}

// taskEvent renders one notifications/tasks working snapshot.
func taskEvent(requestID string) string {
	payload := fmt.Sprintf(
		`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%q},"taskId":%q,"status":"working","ttlMs":86400000,"pollIntervalMs":1000}}`,
		requestID, subTaskID)
	return sseReply(payload)
}

// finalResponse renders the graceful-close response.
func finalResponse(requestID string) string {
	payload := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%q,"result":{"resultType":"complete","_meta":{"io.modelcontextprotocol/subscriptionId":%q}}}`,
		requestID, requestID)
	return sseReply(payload)
}

// TestSubscribeHappyPath proves acknowledgement-first ordering, subset
// delivery, snapshot correlation, and graceful close.
func TestSubscribeHappyPath(t *testing.T) {
	ts := subServer(t, func(id string) []string {
		return []string{ackEvent(id, subTaskID), taskEvent(id), finalResponse(id)}
	})
	client := newSubClient(t, ts.URL)
	var acknowledged []string
	var snapshots []TaskState
	order := []string{}
	err := client.Subscribe(context.Background(), []string{subTaskID}, &SubscribeCallbacks{
		OnAcknowledged: func(authorized []string) error {
			acknowledged = authorized
			order = append(order, "ack")
			return nil
		},
		OnTask: func(state TaskState) error {
			snapshots = append(snapshots, state)
			order = append(order, "task")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(acknowledged) != 1 || acknowledged[0] != subTaskID {
		t.Errorf("authorized subset = %v", acknowledged)
	}
	if len(snapshots) != 1 || snapshots[0].TaskID != subTaskID {
		t.Fatalf("snapshots = %+v", snapshots)
	}
	if len(order) != 2 || order[0] != "ack" || order[1] != "task" {
		t.Errorf("event order = %v, want [ack task]", order)
	}
}

// TestSubscribeTaskSnapshotPayload covers a completed snapshot carrying the
// full terminal result.
func TestSubscribeTaskSnapshotPayload(t *testing.T) {
	ts := subServer(t, func(id string) []string {
		payload := fmt.Sprintf(
			`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%q},%s}}`,
			id, stripOuterBraces(taskCompletedFixture))
		return []string{ackEvent(id, subTaskID), sseReply(payload), finalResponse(id)}
	})
	client := newSubClient(t, ts.URL)
	var got TaskState
	err := client.Subscribe(context.Background(), []string{subTaskID}, &SubscribeCallbacks{
		OnAcknowledged: func([]string) error { return nil },
		OnTask:         func(state TaskState) error { got = state; return nil },
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if got.Status != TaskCompleted || len(got.Result) == 0 {
		t.Fatalf("snapshot = %+v", got)
	}
	if !containsRawLiteral(got.Result, `"text": "The project foundation is complete."`) {
		t.Errorf("terminal result lost content: %s", got.Result)
	}
}

// TestSubscribeAuthorizationSubset proves the acknowledged subset may be
// smaller than the request.
func TestSubscribeAuthorizationSubset(t *testing.T) {
	ts := subServer(t, func(id string) []string { return []string{ackEvent(id)} })
	client := newSubClient(t, ts.URL)
	var authorized []string
	err := client.Subscribe(context.Background(), []string{subTaskID, "other"}, &SubscribeCallbacks{
		OnAcknowledged: func(ids []string) error { authorized = ids; return nil },
		OnTask:         func(TaskState) error { return nil },
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(authorized) != 0 {
		t.Errorf("authorized = %v, want empty", authorized)
	}
}

// TestSubscribeAbruptClose proves a stream that ends without the final
// response returns cleanly and leaves reconciliation to the caller.
func TestSubscribeAbruptClose(t *testing.T) {
	ts := subServer(t, func(id string) []string {
		return []string{ackEvent(id, subTaskID), taskEvent(id)}
	})
	client := newSubClient(t, ts.URL)
	err := client.Subscribe(context.Background(), []string{subTaskID}, &SubscribeCallbacks{
		OnAcknowledged: func([]string) error { return nil },
		OnTask:         func(TaskState) error { return nil },
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
}

// TestSubscribeProtocolViolations covers ack-first ordering, subscription ID
// correlation, undeclared notifications, and duplicate acks.
func TestSubscribeProtocolViolations(t *testing.T) {
	cases := []struct {
		name   string
		events func(id string) []string
	}{
		{"notification before ack", func(id string) []string {
			return []string{taskEvent(id), ackEvent(id, subTaskID)}
		}},
		{"wrong subscription id", func(id string) []string {
			return []string{
				ackEvent(id, subTaskID),
				sseReply(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/tasks","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"other"},"taskId":%q,"status":"working"}}`, subTaskID)),
			}
		}},
		{"undeclared notification", func(id string) []string {
			return []string{
				ackEvent(id, subTaskID),
				sseReply(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%q}}}`, id)),
			}
		}},
		{"duplicate ack", func(id string) []string {
			return []string{ackEvent(id, subTaskID), ackEvent(id, subTaskID)}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := subServer(t, tc.events)
			client := newSubClient(t, ts.URL)
			err := client.Subscribe(context.Background(), []string{subTaskID}, &SubscribeCallbacks{
				OnAcknowledged: func([]string) error { return nil },
				OnTask:         func(TaskState) error { return nil },
			})
			if err == nil {
				t.Fatal("protocol violation accepted")
			}
			var uerr *Error
			if !errors.As(err, &uerr) || uerr.Kind != KindProtocol {
				t.Fatalf("err = %v, want protocol kind", err)
			}
		})
	}
}

// TestSubscribeRejectedRequest covers non-2xx subscription responses.
func TestSubscribeRejectedRequest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, jsonErrorReply("x", -32021, "missing required client capability"))
	}))
	t.Cleanup(ts.Close)
	client := newSubClient(t, ts.URL)
	err := client.Subscribe(context.Background(), []string{subTaskID}, &SubscribeCallbacks{
		OnAcknowledged: func([]string) error { return nil },
		OnTask:         func(TaskState) error { return nil },
	})
	if err == nil {
		t.Fatal("rejected subscription accepted")
	}
}

// TestSubscribeValidation covers client-side parameter checks.
func TestSubscribeValidation(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(ts.Close)
	client := newSubClient(t, ts.URL)
	if err := client.Subscribe(context.Background(), nil, &SubscribeCallbacks{
		OnAcknowledged: func([]string) error { return nil },
		OnTask:         func(TaskState) error { return nil },
	}); err == nil {
		t.Error("empty task list accepted")
	}
	if err := client.Subscribe(context.Background(), []string{subTaskID}, nil); err == nil {
		t.Error("nil callbacks accepted")
	}
}

// TestSubscribeCallbackErrorAborts proves a callback error stops the stream
// and propagates.
func TestSubscribeCallbackErrorAborts(t *testing.T) {
	ts := subServer(t, func(id string) []string {
		return []string{ackEvent(id, subTaskID), taskEvent(id), finalResponse(id)}
	})
	client := newSubClient(t, ts.URL)
	wantErr := errors.New("stop the stream")
	err := client.Subscribe(context.Background(), []string{subTaskID}, &SubscribeCallbacks{
		OnAcknowledged: func([]string) error { return nil },
		OnTask:         func(TaskState) error { return wantErr },
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want callback error", err)
	}
}

// stripOuterBraces removes the outermost braces of a JSON object document so
// its members can be inlined into another object.
func stripOuterBraces(doc string) string {
	trimmed := strings.TrimSpace(doc)
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		return trimmed[1 : len(trimmed)-1]
	}
	return trimmed
}
