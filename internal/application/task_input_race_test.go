package application

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/submission"
)

func TestDeliveredInputPersistsAfterCallerCancel(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	up.onGet = func(int) (int, string) {
		return http.StatusOK, taskState("input_required", "2026-09-11T10:00:04Z", taskInputRequests())
	}
	svc, st := taskApp(t, up)
	out, appErr := svc.Submit(context.Background(), messageInput("cancel-persist"))
	if appErr != nil {
		t.Fatal(appErr.Message)
	}
	waitStatus(t, st, out.SubmissionID, contract.StatusInputRequired)
	svc.tasks.Stop()
	if err := st.SetInputResponse(context.Background(), out.SubmissionID, "approval", json.RawMessage(`{"action":"accept"}`)); err != nil {
		t.Fatal(err)
	}
	owner := "persist-after-cancel"
	name := inputDeliveryLease(out.SubmissionID)
	if ok, err := st.ClaimLease(context.Background(), name, owner, time.Minute); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	generation, owned, err := st.LeaseGeneration(context.Background(), name, owner)
	if err != nil || !owned {
		t.Fatalf("generation: owned=%v err=%v", owned, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if appErr = svc.recordDeliveredInput(ctx, out.SubmissionID, name, owner, generation, []string{"approval"}); appErr != nil {
		t.Fatal(appErr.Message)
	}
	pending, err := st.InputResponsePending(context.Background(), out.SubmissionID, "approval")
	if err != nil || pending {
		t.Fatalf("pending after cancelled caller = %v %v", pending, err)
	}
}

func TestTaskDeliveryLeaseOutlivesSlowUpdate(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	hold := make(chan struct{})
	up.mu.Lock()
	up.holdUpdate = hold
	up.mu.Unlock()
	up.onGet = func(int) (int, string) {
		return http.StatusOK, taskState("input_required", "2026-09-11T10:00:04Z", taskInputRequests())
	}
	svc, st := taskApp(t, up)
	svc.inputDeliveryTTL = 400 * time.Millisecond
	out, appErr := svc.Submit(context.Background(), messageInput("slow-lease"))
	if appErr != nil {
		t.Fatal(appErr.Message)
	}
	waitStatus(t, st, out.SubmissionID, contract.StatusInputRequired)
	svc.tasks.Stop()

	first := make(chan *contract.Error, 1)
	go func() {
		_, err := svc.Await(context.Background(), contract.AwaitInput{
			SubmissionID:   out.SubmissionID,
			TimeoutMS:      1,
			InputResponses: map[string]json.RawMessage{"approval": json.RawMessage(`{"action":"accept"}`)},
		})
		first <- err
	}()
	waitForUpdate(t, up, first)

	second := make(chan *contract.Error, 1)
	go func() {
		_, err := svc.Await(context.Background(), contract.AwaitInput{
			SubmissionID:   out.SubmissionID,
			TimeoutMS:      1,
			InputResponses: map[string]json.RawMessage{"approval": json.RawMessage(`{"action":"accept"}`)},
		})
		second <- err
	}()
	// Longer than the shortened lease, so a holder that does not renew loses it.
	time.Sleep(900 * time.Millisecond)
	if got := up.updateCount(); got != 1 {
		close(hold)
		t.Fatalf("expired delivery lease allowed a second update: %d", got)
	}
	close(hold)
	if err := <-first; err != nil {
		t.Fatalf("first delivery: %s", err.Message)
	}
	if err := <-second; err != nil {
		t.Fatalf("second delivery: %s", err.Message)
	}
	if up.updateCount() != 1 {
		t.Fatalf("updates = %d, want 1", up.updateCount())
	}
}

func waitForUpdate(t *testing.T, up *taskUpstream, delivery <-chan *contract.Error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if up.updateCount() >= 1 {
			return
		}
		select {
		case err := <-delivery:
			if err != nil {
				t.Fatalf("delivery failed before tasks/update: %s", err.Message)
			}
			t.Fatal("delivery returned before tasks/update")
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for tasks/update")
}

func TestTaskSnapshotWaitsForInputDelivery(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	hold := make(chan struct{})
	up.mu.Lock()
	up.holdUpdate = hold
	up.mu.Unlock()
	var completed atomic.Bool
	up.onGet = func(int) (int, string) {
		if !completed.Load() {
			return http.StatusOK, taskState("input_required", "2026-09-11T10:00:04Z", taskInputRequests())
		}
		return http.StatusOK, taskState("completed", "2026-09-11T10:00:09Z", taskSuccessResult(false))
	}
	svc, st := taskApp(t, up)
	out, appErr := svc.Submit(context.Background(), messageInput("snap-wait"))
	if appErr != nil {
		t.Fatal(appErr.Message)
	}
	waitStatus(t, st, out.SubmissionID, contract.StatusInputRequired)

	done := make(chan *contract.Error, 1)
	go func() {
		_, err := svc.Await(context.Background(), contract.AwaitInput{
			SubmissionID:   out.SubmissionID,
			TimeoutMS:      1,
			InputResponses: map[string]json.RawMessage{"approval": json.RawMessage(`{"action":"accept"}`)},
		})
		done <- err
	}()
	waitUntil(t, func() bool { return up.updateCount() == 1 })
	completed.Store(true)
	time.Sleep(80 * time.Millisecond)
	sub, err := st.GetSubmission(context.Background(), out.SubmissionID)
	if err != nil {
		close(hold)
		t.Fatal(err)
	}
	if sub.Status != contract.StatusInputRequired {
		close(hold)
		t.Fatalf("snapshot changed state to %s while input delivery held the lease", sub.Status)
	}
	if !strings.Contains(up.updateBody(), `"approval"`) {
		close(hold)
		t.Fatalf("update lost the outstanding id: %s", up.updateBody())
	}
	close(hold)
	if err := <-done; err != nil {
		t.Fatalf("delivery: %s", err.Message)
	}
	waitStatus(t, st, out.SubmissionID, contract.StatusCompleted)
}

func TestTaskFailureWaitsForInputDelivery(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	hold := make(chan struct{})
	up.mu.Lock()
	up.holdUpdate = hold
	up.mu.Unlock()
	var lookupFailed atomic.Bool
	up.onGet = func(int) (int, string) {
		if !lookupFailed.Load() {
			return http.StatusOK, taskState("input_required", "2026-09-11T10:00:04Z", taskInputRequests())
		}
		return http.StatusBadRequest, `{"code":-32602,"message":"Task not found"}`
	}
	svc, st := taskApp(t, up)
	out, appErr := svc.Submit(context.Background(), messageInput("fail-wait"))
	if appErr != nil {
		t.Fatal(appErr.Message)
	}
	waitStatus(t, st, out.SubmissionID, contract.StatusInputRequired)

	done := make(chan *contract.Error, 1)
	go func() {
		_, err := svc.Await(context.Background(), contract.AwaitInput{
			SubmissionID:   out.SubmissionID,
			TimeoutMS:      1,
			InputResponses: map[string]json.RawMessage{"approval": json.RawMessage(`{"action":"accept"}`)},
		})
		done <- err
	}()
	waitForUpdate(t, up, done)
	lookupFailed.Store(true)
	time.Sleep(120 * time.Millisecond)
	sub, err := st.GetSubmission(context.Background(), out.SubmissionID)
	if err != nil {
		close(hold)
		t.Fatal(err)
	}
	if submission.Terminal(sub.Status) {
		close(hold)
		t.Fatalf("failure terminalized %s while input delivery held the lease", sub.Status)
	}
	if !strings.Contains(up.updateBody(), `"approval"`) {
		close(hold)
		t.Fatalf("update body = %s", up.updateBody())
	}
	close(hold)
	if err := <-done; err != nil {
		t.Fatalf("delivery: %s", err.Message)
	}
	waitStatus(t, st, out.SubmissionID, contract.StatusFailed)
}
