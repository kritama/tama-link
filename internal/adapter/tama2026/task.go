package tama2026

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/upstream"
)

// ErrTaskUnavailable reports that an owner-bound task lookup failed in a
// way that must not distinguish a missing task from an unauthorized one.
var ErrTaskUnavailable = errors.New("tama2026: task unavailable")

// ErrSubscriptionUnavailable reports that task notifications cannot be
// used for this connection. Polling remains the recovery path.
var ErrSubscriptionUnavailable = errors.New("tama2026: task subscription unavailable")

// ErrTaskNotSubscribed reports that the acknowledgement did not authorize
// the requested task ID. The caller must poll.
var ErrTaskNotSubscribed = errors.New("tama2026: task not in the authorized subscription set")

// TaskSnapshot is one validated task observation with upstream wire details
// already projected to the normalized contract. TTL and poll interval stay
// the validated millisecond integers; converting them to time.Duration at
// this boundary would wrap values the Tasks schema allows.
type TaskSnapshot struct {
	TaskID         string
	Status         contract.Status
	StatusMessage  string
	UpdatedAt      string
	TTLMs          int64
	PollIntervalMs int64
	Result         *contract.Result
	Failure        *contract.Error
	InputRequests  json.RawMessage
	Capabilities   json.RawMessage
	// TerminalEvidence is the lossless upstream failure or cancellation
	// document. It is not part of the downstream await contract.
	TerminalEvidence json.RawMessage
}

// OpenTask issues the server-directed tools/call for one pinned task
// operation and returns the working-task handle. The initial result must
// be working; a terminal initial state is a protocol failure.
func (cn *Connection) OpenTask(ctx context.Context, name string, args json.RawMessage) (*TaskSnapshot, error) {
	resp, err := cn.Execute(ctx, name, args)
	if err != nil {
		return nil, err
	}
	snap := &TaskSnapshot{
		TaskID:         resp.TaskID,
		Status:         contract.StatusRunning,
		StatusMessage:  resp.StatusMessage,
		UpdatedAt:      resp.LastUpdatedAt,
		TTLMs:          resp.TTLMs,
		PollIntervalMs: resp.PollIntervalMs,
		Capabilities:   capabilitySnapshot(cn),
	}
	if snap.TaskID == "" || resp.Status != upstream.TaskWorking {
		return nil, fmt.Errorf("%w: initial task handle is incomplete", ErrProtocolMismatch)
	}
	return snap, nil
}

// GetTask reads the detailed owner-bound task state. maxResponseBytes, when
// positive, bounds this one response. Missing and unauthorized tasks return
// ErrTaskUnavailable and are otherwise indistinguishable.
func (cn *Connection) GetTask(ctx context.Context, taskID string, maxResponseBytes int64) (*TaskSnapshot, error) {
	state, err := cn.adapter.upstream.TaskGetWithin(ctx, taskID, maxResponseBytes)
	if err != nil {
		return nil, classifyTaskLookup(err)
	}
	return normalizeTaskState(state, capabilitySnapshot(cn))
}

// UpdateTask sends one tasks/update for outstanding input responses. The
// acknowledgement is not proof that the task left input_required.
func (cn *Connection) UpdateTask(ctx context.Context, taskID string, responses json.RawMessage) error {
	if _, err := cn.adapter.upstream.TaskUpdate(ctx, taskID, responses); err != nil {
		return classifyTaskLookup(err)
	}
	return nil
}

// WatchTask subscribes to one task ID and delivers complete snapshots. The
// acknowledgement must authorize that ID before any snapshot is delivered.
// A stream end, including cancellation, is not itself a task failure.
func (cn *Connection) WatchTask(ctx context.Context, taskID string, on func(TaskSnapshot) error) error {
	if on == nil {
		return errors.New("task snapshot callback is required")
	}
	err := cn.adapter.upstream.Subscribe(ctx, []string{taskID}, &upstream.SubscribeCallbacks{
		OnAcknowledged: func(authorized []string) error {
			for _, id := range authorized {
				if id == taskID {
					return nil
				}
			}
			return ErrTaskNotSubscribed
		},
		OnTask: func(state upstream.TaskState) error {
			snap, nerr := normalizeTaskState(&state, capabilitySnapshot(cn))
			if nerr != nil {
				return nerr
			}
			return on(*snap)
		},
	})
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrTaskNotSubscribed) {
		return err
	}
	if subscriptionUnavailable(err) {
		return fmt.Errorf("%w: %v", ErrSubscriptionUnavailable, err)
	}
	return classify(err)
}

func capabilitySnapshot(cn *Connection) json.RawMessage {
	raw, err := json.Marshal(struct {
		Client json.RawMessage `json:"client"`
		Server json.RawMessage `json:"server"`
	}{
		Client: tasksCapabilities,
		Server: cn.serverCapabilities,
	})
	if err != nil {
		return nil
	}
	return raw
}

func classifyTaskLookup(err error) error {
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.KindProtocol && ue.Code == -32602 {
		return fmt.Errorf("%w: %v", ErrTaskUnavailable, err)
	}
	return classify(err)
}

func subscriptionUnavailable(err error) bool {
	var ue *upstream.Error
	if !errors.As(err, &ue) || ue.Kind != upstream.KindProtocol {
		return false
	}
	// method-not-found, or the client did not declare the notification capability.
	return ue.Code == -32601 || ue.Code == -32021
}

func normalizeTaskState(state *upstream.TaskState, capabilities json.RawMessage) (*TaskSnapshot, error) {
	if state == nil || state.TaskID == "" {
		return nil, fmt.Errorf("%w: task state has no id", ErrProtocolMismatch)
	}
	snap := &TaskSnapshot{
		TaskID:         state.TaskID,
		StatusMessage:  state.StatusMessage,
		UpdatedAt:      state.LastUpdatedAt,
		TTLMs:          state.TTLMs,
		PollIntervalMs: state.PollIntervalMs,
		Capabilities:   capabilities,
	}
	switch state.Status {
	case upstream.TaskWorking:
		snap.Status = contract.StatusRunning
	case upstream.TaskInputRequired:
		snap.Status = contract.StatusInputRequired
		snap.InputRequests = state.InputRequests
	case upstream.TaskCompleted:
		result, err := NormalizeCompleteResult(state.Result)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrProtocolMismatch, err)
		}
		snap.Status = contract.StatusCompleted
		snap.Result = &result
	case upstream.TaskFailed:
		snap.TerminalEvidence = append(json.RawMessage(nil), state.Error...)
		if expirationError(state.Error) {
			snap.Status = contract.StatusExpired
			failure := contract.NewError(contract.CodeUpstreamExecutionFailed,
				"The upstream task expired before it completed.")
			snap.Failure = &failure
			break
		}
		snap.Status = contract.StatusFailed
		failure := contract.NewError(contract.CodeUpstreamExecutionFailed, "The upstream task failed.")
		snap.Failure = &failure
	case upstream.TaskCancelled:
		snap.Status = contract.StatusCancelled
		snap.TerminalEvidence = append(json.RawMessage(nil), state.Raw...)
		failure := contract.NewError(contract.CodeUpstreamExecutionFailed, "The upstream task was cancelled.")
		snap.Failure = &failure
	default:
		return nil, fmt.Errorf("%w: unknown task status %q", ErrProtocolMismatch, state.Status)
	}
	return snap, nil
}

func expirationError(raw json.RawMessage) bool {
	var view struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &view) != nil {
		return false
	}
	if strings.Contains(strings.ToLower(view.Message), "expired") {
		return true
	}
	code, ok := view.Code.(string)
	return ok && strings.EqualFold(code, "expired")
}
