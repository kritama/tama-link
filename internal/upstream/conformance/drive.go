package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/kritama/tama-link/internal/upstream"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// invocation is one adapted client call.
type invocation struct {
	raw    json.RawMessage
	acked  []string
	sawAck bool
	tasks  []upstream.TaskState
	err    error
}

// runFixture adapts one pinned fixture through the upstream client.
func runFixture(t *testing.T, fx Fixture) {
	t.Helper()
	if fx.legacy() {
		return
	}
	if fx.Name == "tools/call missing mirrored parameter header" {
		assertMirroredHeaderIsRequired(t, fx)
		return
	}
	if rejectedLocally(fx) {
		assertLocalRejection(t, fx)
		return
	}
	server := newReplay(fx)
	defer server.close()
	client := newClient(t, server.url())
	got := invoke(t, client, fx)
	if len(server.requests) != 1 {
		t.Fatalf("%s emitted %d requests", fx.Name, len(server.requests))
	}
	observe(t, fx, server.requests[0].Header, server.requests[0].Body)
	assertOutcome(t, fx, got)
}

func rejectedLocally(fx Fixture) bool {
	if fx.Method() != upstream.MethodTaskUpdate || fx.RequestValid {
		return false
	}
	params, err := fx.params()
	return err == nil && !jsonObject(params.InputResponses)
}

func assertLocalRejection(t *testing.T, fx Fixture) {
	t.Helper()
	var calls int
	client := newClientTransport(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("upstream must not be called")
	}))
	params, err := fx.params()
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.TaskUpdate(context.Background(), params.TaskID, params.InputResponses)
	if calls != 0 {
		t.Fatalf("invalid input responses reached upstream (%d calls)", calls)
	}
	if err == nil || err.Error() != "input responses must be a JSON object" {
		t.Fatalf("got %v, want local validation error", err)
	}
}

func newClient(t *testing.T, endpoint string) *upstream.Client {
	t.Helper()
	client, err := upstream.New(upstream.Config{
		Endpoint:           endpoint,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.0.0-conformance"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "conformance-token", nil },
		MaxResponseBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func invoke(t *testing.T, client *upstream.Client, fx Fixture) invocation {
	t.Helper()
	ctx := context.Background()
	params, err := fx.params()
	if err != nil {
		t.Fatal(err)
	}
	var got invocation
	switch fx.Method() {
	case upstream.MethodDiscover:
		var result *upstream.DiscoverResult
		result, got.err = client.Discover(ctx, 0)
		if result != nil {
			got.raw = result.Raw
		}
	case upstream.MethodListTools:
		var result *upstream.ListToolsResult
		result, got.err = client.ListTools(ctx, "", 0)
		if result != nil {
			got.raw = result.Raw
		}
	case upstream.MethodCallTool:
		var result *upstream.CallToolResponse
		headers, herr := paramHeadersFor(params.Name)
		if herr != nil {
			t.Fatal(herr)
		}
		result, got.err = client.CallTool(ctx, &upstream.CallToolParams{
			Name:         params.Name,
			Arguments:    params.Arguments,
			Capabilities: callCapabilities(fx, params.Name),
			ParamHeaders: headers,
		})
		if result != nil {
			got.raw = result.Raw
		}
	case upstream.MethodTaskGet:
		var state *upstream.TaskState
		state, got.err = client.TaskGet(ctx, params.TaskID)
		if state != nil {
			got.raw = state.Raw
		}
	case upstream.MethodTaskUpdate:
		got.raw, got.err = client.TaskUpdate(ctx, params.TaskID, params.InputResponses)
	case upstream.MethodTaskCancel:
		got.raw, got.err = client.TaskCancel(ctx, params.TaskID)
	case upstream.MethodSubscribe:
		ids := subscriptionIDs(params.Notifications.TaskIDs)
		got.err = client.Subscribe(ctx, ids, &upstream.SubscribeCallbacks{
			OnAcknowledged: func(ids []string) error {
				got.sawAck = true
				got.acked = append([]string(nil), ids...)
				return nil
			},
			OnTask: func(state upstream.TaskState) error {
				got.tasks = append(got.tasks, state)
				return nil
			},
		})
	default:
		t.Fatalf("no client adapter for %s", fx.Method())
	}
	return got
}

func callCapabilities(fx Fixture, name string) json.RawMessage {
	if mustDeclareTasks(fx, name) && fx.Method() == upstream.MethodCallTool {
		return json.RawMessage(`{"extensions":{"io.modelcontextprotocol/tasks":{}}}`)
	}
	return nil
}

func assertOutcome(t *testing.T, fx Fixture, got invocation) {
	t.Helper()
	if fx.Expected.Status == 401 || fx.Expected.Status == 403 {
		if !upstream.IsAuth(got.err) {
			t.Fatalf("status %d: got %v, want authorization rejection", fx.Expected.Status, got.err)
		}
		return
	}
	if code, failed := expectedErrorCode(fx); failed {
		var ue *upstream.Error
		if !errors.As(got.err, &ue) || ue.Kind != upstream.KindProtocol || ue.Code != code {
			t.Fatalf("status %d: got %v, want protocol error %d", fx.Expected.Status, got.err, code)
		}
		return
	}
	if got.err != nil {
		t.Fatalf("fixture success returned %v", got.err)
	}
	if fx.Method() == upstream.MethodSubscribe {
		assertStream(t, fx, got)
		return
	}
	if err := sameResult(got.raw, expectedResult(fx)); err != nil {
		t.Fatalf("decoded result: %v", err)
	}
}

func expectedErrorCode(fx Fixture) (int, bool) {
	var body struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(fx.Expected.Body, &body) != nil || body.Error == nil {
		return 0, false
	}
	return body.Error.Code, true
}

func expectedResult(fx Fixture) json.RawMessage {
	var body struct {
		Result json.RawMessage `json:"result"`
	}
	_ = json.Unmarshal(fx.Expected.Body, &body)
	return body.Result
}

func assertStream(t *testing.T, fx Fixture, got invocation) {
	t.Helper()
	if !got.sawAck {
		t.Fatal("subscription produced no acknowledgement")
	}
	wantIDs := acknowledgementIDs(t, fx)
	if stringsJoin(got.acked) != stringsJoin(sortedCopy(wantIDs)) {
		t.Fatalf("acknowledged %v, fixture %v", got.acked, wantIDs)
	}
	notifications := taskNotifications(t, fx)
	if len(got.tasks) != len(notifications) {
		t.Fatalf("task snapshots = %d, fixture notifications = %d", len(got.tasks), len(notifications))
	}
	for i, state := range got.tasks {
		var want struct {
			TaskID string `json:"taskId"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(notifications[i], &want); err != nil {
			t.Fatal(err)
		}
		if state.TaskID != want.TaskID || state.Status != want.Status {
			t.Fatalf("snapshot %s/%s, fixture %s/%s", state.TaskID, state.Status, want.TaskID, want.Status)
		}
	}
}
