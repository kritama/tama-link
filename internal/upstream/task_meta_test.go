package upstream

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestTaskMethodsDeclareTasksCapability proves task lookup does not depend
// on the client's default capability set. Discover keeps that default.
func TestTaskMethodsDeclareTasksCapability(t *testing.T) {
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		if rec.Method == MethodDiscover {
			return 200, "application/json", jsonReply(rec.BodyID, discoverFixture)
		}
		return 200, "application/json", jsonReply(rec.BodyID, taskWorkingFixture)
	})
	client, err := New(Config{
		Endpoint:           ts.URL,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "test-token", nil },
		MaxResponseBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Discover(context.Background(), 0); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := string(ts.requests[0].Meta["io.modelcontextprotocol/clientCapabilities"]); got != `{}` {
		t.Fatalf("discover capabilities = %s, want the empty default", got)
	}
	if _, err := client.TaskGet(context.Background(), "d59d7f2a-933e-44f4-8c28-4d28e9f0d937"); err != nil {
		t.Fatalf("TaskGet: %v", err)
	}
	got := string(ts.requests[1].Meta["io.modelcontextprotocol/clientCapabilities"])
	if !strings.Contains(got, "io.modelcontextprotocol/tasks") {
		t.Fatalf("tasks/get capabilities = %s, want the Tasks extension", got)
	}
}
