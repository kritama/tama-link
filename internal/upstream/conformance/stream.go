package conformance

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// subscriptionIDs adapts a fixture task-id list to a request the client
// can send. Empty lists and duplicates are server-fixture defects; Link does
// not emit them. An empty list still needs one id so an authorization
// rejection can be classified.
func subscriptionIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return []string{"conformance-task"}
	}
	return out
}

func acknowledgementIDs(t *testing.T, fx Fixture) []string {
	t.Helper()
	if len(fx.Expected.Events) == 0 {
		t.Fatal("stream fixture has no events")
	}
	var event struct {
		Params struct {
			Notifications struct {
				TaskIDs []string `json:"taskIds"`
			} `json:"notifications"`
		} `json:"params"`
	}
	if err := json.Unmarshal(fx.Expected.Events[0], &event); err != nil {
		t.Fatal(err)
	}
	return event.Params.Notifications.TaskIDs
}

func taskNotifications(t *testing.T, fx Fixture) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	for _, event := range fx.Expected.Events {
		var view struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(event, &view) != nil {
			continue
		}
		if view.Method == "notifications/tasks" {
			var envelope struct {
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(event, &envelope) != nil {
				t.Fatal("task notification has no params")
			}
			out = append(out, envelope.Params)
		}
	}
	return out
}

func sortedCopy(ids []string) []string {
	out := append([]string(nil), ids...)
	slices.Sort(out)
	return out
}

func stringsJoin(ids []string) string {
	return strings.Join(ids, "\x00")
}
