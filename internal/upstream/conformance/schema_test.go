package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kritama/tama-link/internal/upstream"
)

// TestTaskSchemaFixtures runs the static positive and negative task values
// through tasks/get. Invalid cross-state payloads and unsafe integers must
// fail closed.
func TestTaskSchemaFixtures(t *testing.T) {
	set := mustLoad(t)
	for _, fx := range set.Schema {
		t.Run(fx.Name, func(t *testing.T) {
			t.Parallel()
			runSchemaFixture(t, fx)
		})
	}
}

func runSchemaFixture(t *testing.T, fx SchemaFixture) {
	t.Helper()
	taskID := schemaTaskID(t, fx.Value)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := rewriteID(fx.Value, "", "")
		var id string
		var envelope struct {
			ID string `json:"id"`
		}
		raw, _ := readBody(r)
		_ = json.Unmarshal(raw, &envelope)
		id = envelope.ID
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + jsonString(id) + `,"result":`))
		_, _ = w.Write(body)
		_, _ = w.Write([]byte(`}`))
	}))
	defer server.Close()
	client := newClient(t, server.URL)
	state, err := client.TaskGet(context.Background(), taskID)
	if fx.Valid {
		if err != nil {
			t.Fatalf("valid task profile rejected: %v", err)
		}
		if state.TaskID != taskID {
			t.Fatalf("task id %q, fixture %q", state.TaskID, taskID)
		}
		return
	}
	if !upstream.IsProtocol(err) {
		t.Fatalf("invalid task profile returned %v, want protocol failure", err)
	}
}

func schemaTaskID(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var view struct {
		TaskID string `json:"taskId"`
	}
	if err := json.Unmarshal(raw, &view); err != nil || view.TaskID == "" {
		t.Fatalf("schema fixture has no task id: %v", err)
	}
	return view.TaskID
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
