package application

import (
	"context"
	"errors"
	"time"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/submission"
)

func (r *taskRunner) apply(
	ctx context.Context,
	sub *store.Submission,
	leaseName, leaseOwner string,
	snap *tama2026.TaskSnapshot,
) (bool, error) {
	if snap == nil || snap.TaskID == "" || (sub.TaskID != "" && snap.TaskID != sub.TaskID) {
		return false, r.fail(ctx, sub.ID, leaseName, leaseOwner, classify(tama2026.ErrProtocolMismatch))
	}
	if !newerObservation(sub.TaskUpdatedAt, snap.UpdatedAt) && sub.TaskUpdatedAt != "" {
		return submission.Terminal(sub.Status), nil
	}
	// Snapshot writes change the outstanding input set. They wait out an
	// in-flight tasks/update instead of clearing that set underneath it.
	release, held, err := claimInputDeliveryOnce(ctx, r.store, sub.ID)
	if err != nil {
		if errors.Is(err, store.ErrBusy) {
			return false, nil
		}
		return false, err
	}
	if !held {
		return false, nil
	}
	defer release()
	if err := r.observe(ctx, sub.ID, leaseName, leaseOwner, snap); err != nil {
		if errors.Is(err, store.ErrConcurrentUpdate) || errors.Is(err, store.ErrLeaseNotOwned) {
			return false, nil
		}
		return false, err
	}
	if snap.Status == sub.Status {
		return false, nil
	}
	switch snap.Status {
	case contract.StatusCompleted:
		if snap.Result == nil {
			return false, r.fail(ctx, sub.ID, leaseName, leaseOwner, classify(tama2026.ErrProtocolMismatch))
		}
		_, err := r.store.CompleteLeased(ctx, sub.ID, leaseName, leaseOwner, *snap.Result)
		if err != nil && !errors.Is(err, store.ErrResultTooLarge) {
			return false, err
		}
		r.note(ctx, sub, contract.StatusCompleted, snap.StatusMessage)
		return true, nil
	case contract.StatusRunning, contract.StatusInputRequired:
		if !submission.Allowed(sub.Status, snap.Status) {
			return false, nil
		}
		next, err := r.store.TransitionLeased(ctx, sub.ID, leaseName, leaseOwner, snap.Status, store.TransitionDetail{})
		if err != nil {
			return false, err
		}
		r.note(ctx, next, snap.Status, snap.StatusMessage)
		return false, nil
	case contract.StatusFailed, contract.StatusCancelled, contract.StatusExpired:
		if snap.Failure == nil {
			return false, r.fail(ctx, sub.ID, leaseName, leaseOwner, classify(tama2026.ErrProtocolMismatch))
		}
		if !submission.Allowed(sub.Status, snap.Status) {
			return submission.Terminal(sub.Status), nil
		}
		if _, err := r.store.CaptureTerminalLeased(ctx, sub.ID, leaseName, leaseOwner, snap.Status, *snap.Failure, snap.TerminalEvidence); err != nil {
			return false, err
		}
		r.note(ctx, sub, snap.Status, snap.StatusMessage)
		return true, nil
	default:
		return false, r.fail(ctx, sub.ID, leaseName, leaseOwner, classify(tama2026.ErrProtocolMismatch))
	}
}

func (r *taskRunner) observe(ctx context.Context, id, leaseName, leaseOwner string, snap *tama2026.TaskSnapshot) error {
	record := store.TaskRecord{
		TTLMs:          snap.TTLMs,
		PollIntervalMs: snap.PollIntervalMs,
		Capabilities:   string(snap.Capabilities),
		UpdatedAt:      snap.UpdatedAt,
	}
	if snap.Status == contract.StatusInputRequired {
		record.InputRequests = snap.InputRequests
	}
	return r.store.SaveTaskRecordLeased(ctx, id, leaseName, leaseOwner, record)
}

func (r *taskRunner) note(ctx context.Context, sub *store.Submission, state contract.Status, message string) {
	if len(message) > 256 {
		message = message[:256]
	}
	event := contract.Event{
		SubmissionID: sub.ID,
		Sequence:     sub.Sequence + 1,
		Timestamp:    r.now().UTC(),
		State:        state,
		Message:      message,
	}
	_, _ = r.store.AppendEvents(ctx, sub.ID, []contract.Event{event})
}

func newerObservation(prev, next string) bool {
	if next == "" {
		return prev == ""
	}
	if prev == "" {
		return true
	}
	pt, perr := time.Parse(time.RFC3339, prev)
	nt, nerr := time.Parse(time.RFC3339, next)
	if perr != nil || nerr != nil {
		return next >= prev
	}
	// Equal timestamps stay eligible. A status change can share the previous
	// second-precision stamp, and reapplying an unchanged snapshot is a no-op.
	return !nt.Before(pt)
}
