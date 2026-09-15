package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

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
	params, err := json.Marshal(wireSubscribe{Notifications: wireSubscribeNotifications{TaskIDs: taskIDs}})
	if err != nil {
		return fmt.Errorf("encode subscriptions/listen params: %w", err)
	}
	id, resp, err := c.doRequest(ctx, MethodSubscribe, "", params, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return c.readHTTPError(resp)
	}
	if baseMediaType(resp.Header.Get("Content-Type")) != "text/event-stream" {
		drain(resp.Body)
		return newError(KindTransport, 0, fmt.Errorf("subscription stream has content type %q", resp.Header.Get("Content-Type")))
	}
	return c.readSubscription(id, resp.Body, cb)
}

// wireSubscribe is the subscriptions/listen params object.
type wireSubscribe struct {
	Notifications wireSubscribeNotifications `json:"notifications"`
}

type wireSubscribeNotifications struct {
	TaskIDs []string `json:"taskIds"`
}

// readSubscription consumes the stream: acknowledgement first, then task
// snapshots correlated by subscription ID, and an optional final response.
func (c *Client) readSubscription(subscriptionID string, body io.ReadCloser, cb *SubscribeCallbacks) error {
	acknowledged := false
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
				authorized, err := parseAcknowledgement(req.Params, subscriptionID)
				if err != nil {
					return true, err
				}
				acknowledged = true
				return false, cb.OnAcknowledged(authorized)
			case notificationTasks:
				if !acknowledged {
					return true, newError(KindProtocol, 0, fmt.Errorf("task notification before acknowledgement"))
				}
				state, err := parseTaskNotification(req.Params, subscriptionID)
				if err != nil {
					return true, err
				}
				return false, cb.OnTask(*state)
			default:
				return true, newError(KindProtocol, 0, fmt.Errorf("undeclared notification type on subscription stream"))
			}
		}
		// The final JSON-RPC response marks graceful closure.
		if resp, ok := msg.(*jsonrpc.Response); ok && idMatches(resp.ID, subscriptionID) {
			return true, nil
		}
		return true, newError(KindProtocol, 0, fmt.Errorf("unexpected message on subscription stream"))
	})
}

// parseAcknowledgement validates the acknowledgement event and returns the
// authorized task ID subset.
func parseAcknowledgement(params json.RawMessage, subscriptionID string) ([]string, error) {
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
	authorized := view.Notifications.TaskIDs
	if authorized == nil {
		authorized = []string{}
	}
	return authorized, nil
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
	state, err := decodeTaskState(params, "")
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
