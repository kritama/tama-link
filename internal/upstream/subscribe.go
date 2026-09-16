package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// MethodSubscribe opens a task notification stream.
const MethodSubscribe = "subscriptions/listen"

// Task notifications on a subscription stream.
const (
	notificationSubscriptionsAcknowledged = "notifications/subscriptions/acknowledged"
	notificationTasks                     = "notifications/tasks"
)

// metaKeySubscriptionID tags every stream event with its subscription.
const metaKeySubscriptionID = "io.modelcontextprotocol/subscriptionId"

// SubscribeCallbacks receives one subscription stream. Errors returned from
// a callback abort the stream.
type SubscribeCallbacks struct {
	// OnAcknowledged receives the authorized task ID subset. The
	// acknowledgement is always the first stream event; its subset may be
	// smaller than the request when some IDs are not authorized for the
	// caller.
	OnAcknowledged func(authorized []string) error
	// OnTask receives each complete task snapshot. Snapshots are the same
	// detailed state tasks/get would return.
	OnTask func(TaskState) error
}

// Subscribe opens a task-ID subscription stream for the given task IDs and
// dispatches events until the stream closes or ctx is cancelled. It returns
// nil when the stream ends (gracefully or abruptly); both cases require the
// caller to reconcile through TaskGet. Non-2xx responses and protocol
// violations return classified errors.
//
// Every event is bound to the requested task IDs: the acknowledgement must
// authorize only a subset of the request, and every later task snapshot must
// carry an acknowledged ID. A matching subscription ID alone is not
// sufficient to deliver an unrequested task.
//
// Correctness must never depend on observing every notification: dropped
// events, overflow closes, and credential expiry all end the stream and
// recovery is always tasks/get.
func (c *Client) Subscribe(ctx context.Context, taskIDs []string, cb *SubscribeCallbacks) error {
	if cb == nil || cb.OnAcknowledged == nil || cb.OnTask == nil {
		return fmt.Errorf("subscription callbacks are required")
	}
	if len(taskIDs) == 0 {
		return fmt.Errorf("at least one task id is required")
	}
	requested := make(map[string]bool, len(taskIDs))
	for _, id := range taskIDs {
		if id == "" {
			return fmt.Errorf("task ids must be non-empty")
		}
		if requested[id] {
			return fmt.Errorf("duplicate task id %q in subscription request", id)
		}
		requested[id] = true
	}
	params, err := json.Marshal(wireSubscribe{Notifications: wireSubscribeNotifications{TaskIDs: taskIDs}})
	if err != nil {
		return fmt.Errorf("encode subscriptions/listen params: %w", err)
	}
	id, resp, err := c.doRequest(ctx, MethodSubscribe, "", params, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// A rejected stream is an authentication failure exactly like a
		// rejected request: callers check IsAuth to refresh or request
		// reauthorization before giving up on the subscription path.
		drain(resp.Body)
		return newError(KindAuth, resp.StatusCode, nil)
	}
	if resp.StatusCode != http.StatusOK {
		return c.readHTTPError(resp)
	}
	if baseMediaType(resp.Header.Get("Content-Type")) != "text/event-stream" {
		drain(resp.Body)
		return newError(KindTransport, 0, fmt.Errorf("subscription stream has content type %q", resp.Header.Get("Content-Type")))
	}
	return c.readSubscription(id, requested, resp.Body, cb)
}

// wireSubscribe is the subscriptions/listen params object.
type wireSubscribe struct {
	Notifications wireSubscribeNotifications `json:"notifications"`
}

type wireSubscribeNotifications struct {
	TaskIDs []string `json:"taskIds"`
}

// readSubscription consumes the stream: an acknowledgement that authorizes a
// subset of the requested task IDs, task snapshots bound to that subset, and
// an optional final response that may only follow the acknowledgement. A
// matching final JSON-RPC error is a protocol failure, not a graceful close.
func (c *Client) readSubscription(subscriptionID string, requested map[string]bool, body io.ReadCloser, cb *SubscribeCallbacks) error {
	acknowledged := false
	authorized := make(map[string]bool)
	return c.scanSSE(body, func(msg jsonrpc.Message) (bool, error) {
		req, ok := msg.(*jsonrpc.Request)
		if ok {
			if req.ID.IsValid() {
				return true, newError(KindProtocol, 0, fmt.Errorf("subscription stream carries a server request"))
			}
			switch req.Method {
			case notificationSubscriptionsAcknowledged:
				if acknowledged {
					return true, newError(KindProtocol, 0, fmt.Errorf("duplicate subscription acknowledgement"))
				}
				ids, err := parseAcknowledgement(req.Params, subscriptionID, requested)
				if err != nil {
					return true, err
				}
				authorized = ids
				acknowledged = true
				return false, cb.OnAcknowledged(sortedKeys(ids))
			case notificationTasks:
				if !acknowledged {
					return true, newError(KindProtocol, 0, fmt.Errorf("task notification before acknowledgement"))
				}
				state, err := parseTaskNotification(req.Params, subscriptionID)
				if err != nil {
					return true, err
				}
				if !authorized[state.TaskID] {
					return true, newError(KindProtocol, 0, fmt.Errorf("task notification outside the acknowledged set"))
				}
				return false, cb.OnTask(*state)
			default:
				return true, newError(KindProtocol, 0, fmt.Errorf("undeclared notification type on subscription stream"))
			}
		}
		// The final JSON-RPC response marks graceful closure; it must follow
		// the acknowledgement. A final error is a protocol failure, not a
		// clean close.
		if resp, ok := msg.(*jsonrpc.Response); ok && idMatches(resp.ID, subscriptionID) {
			if !acknowledged {
				return true, newError(KindProtocol, 0, fmt.Errorf("final response before acknowledgement"))
			}
			if werr, isErr := asWireError(resp); isErr {
				return true, protocolError(werr)
			}
			return true, nil
		}
		return true, newError(KindProtocol, 0, fmt.Errorf("unexpected message on subscription stream"))
	})
}

// parseAcknowledgement validates the acknowledgement event: it must name the
// stream, carry a non-duplicate subset of the requested task IDs, and return
// that subset as the authorized set.
func parseAcknowledgement(params json.RawMessage, subscriptionID string, requested map[string]bool) (map[string]bool, error) {
	var view struct {
		Meta          map[string]json.RawMessage `json:"_meta"`
		Notifications struct {
			TaskIDs []string `json:"taskIds"`
		} `json:"notifications"`
	}
	if err := json.Unmarshal(params, &view); err != nil {
		return nil, newError(KindProtocol, 0, fmt.Errorf("decode subscription acknowledgement"))
	}
	if err := checkSubscriptionID(view.Meta, subscriptionID); err != nil {
		return nil, err
	}
	authorized := make(map[string]bool, len(view.Notifications.TaskIDs))
	for _, id := range view.Notifications.TaskIDs {
		if !requested[id] {
			return nil, newError(KindProtocol, 0, fmt.Errorf("acknowledgement authorizes an unrequested task id"))
		}
		if authorized[id] {
			return nil, newError(KindProtocol, 0, fmt.Errorf("acknowledgement repeats a task id"))
		}
		authorized[id] = true
	}
	return authorized, nil
}

// sortedKeys renders a task ID set in deterministic order for the callback.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// parseTaskNotification validates one notifications/tasks event and projects
// the complete task snapshot.
func parseTaskNotification(params json.RawMessage, subscriptionID string) (*TaskState, error) {
	var envelope struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(params, &envelope); err != nil {
		return nil, newError(KindProtocol, 0, fmt.Errorf("decode task notification"))
	}
	if err := checkSubscriptionID(envelope.Meta, subscriptionID); err != nil {
		return nil, err
	}
	state, err := decodeTaskState(params, "", false)
	if err != nil {
		return nil, err
	}
	// The stream _meta is correlation data, not task state: the retained
	// snapshot mirrors what tasks/get would return.
	if stripped, err := stripMeta(params); err == nil {
		state.Raw = stripped
	}
	return state, nil
}

// stripMeta returns the object without its _meta member. Other members keep
// their raw JSON; key order becomes sorted, which is deterministic.
func stripMeta(raw json.RawMessage) (json.RawMessage, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	delete(members, "_meta")
	return json.Marshal(members)
}

// checkSubscriptionID verifies an event carries the stream's subscription ID.
func checkSubscriptionID(meta map[string]json.RawMessage, subscriptionID string) error {
	raw, ok := meta[metaKeySubscriptionID]
	if !ok {
		return newError(KindProtocol, 0, fmt.Errorf("event has no subscription id"))
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil || id != subscriptionID {
		return newError(KindProtocol, 0, fmt.Errorf("event subscription id mismatch"))
	}
	return nil
}
