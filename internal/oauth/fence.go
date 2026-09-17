package oauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kritama/tama-link/internal/store"
)

// newRefreshSlotLabel returns one unique secure-backend label for a
// fenced credential slot. Labels are per-transaction: two writers that
// read the same fence must never share a slot, or a rejected writer's
// cleanup could delete the winner's live credential.
func newRefreshSlotLabel() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate credential slot label: %w", err)
	}
	return labelRefresh + "@" + hex.EncodeToString(b[:]), nil
}

// newClientSlotLabel returns one unique secure-backend label for a client
// record awaiting its fence commit.
func newClientSlotLabel() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate client slot label: %w", err)
	}
	return store.ClientFenceName + "@" + hex.EncodeToString(b[:]), nil
}

// fencedCredential is one refresh credential plus the generations and
// slots a replacement commits at: the credential-fence generation the
// replacement advances, the lease ownership epoch it must still hold, and
// the previous live slot to retire after the commit.
type fencedCredential struct {
	credential *refreshCredential
	generation int64
	// leaseGeneration is the refresh lease ownership epoch captured when
	// the caller claimed the lease; the fence commit is bound to it.
	leaseGeneration int64
	// previousSlot is the secure-backend label the credential was loaded
	// from. A successful commit retires it, so at most one live slot
	// holds the grant at any time.
	previousSlot string
}

// loadFenced loads the live refresh credential. When no fence has been
// committed yet, the legacy single-label credential is adopted as
// generation 1. The returned generations are what a replacement commits
// at, and the returned slot is what a successful commit retires.
func (c *Client) loadFenced(ctx context.Context) (*fencedCredential, error) {
	generation, slot, found, err := c.lease.ReadCredentialFence(ctx, store.RefreshFenceName)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	if !found {
		// A durable invalidation marker outlives a failed legacy deletion:
		// the rejected grant at the legacy label must not count as live.
		invalidated, err := c.lease.RefreshCredentialInvalidated(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
		}
		if invalidated {
			return nil, nil
		}
		cred, ok, err := c.loadRefresh()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		return &fencedCredential{credential: cred, generation: 1, previousSlot: labelRefresh}, nil
	}
	data, ok, err := c.secrets.GetSecret(slot)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	if !ok {
		return nil, fmt.Errorf("%w: credential slot is missing", ErrBackendUnavailable)
	}
	var cred refreshCredential
	if err := json.Unmarshal(data, &cred); err != nil {
		return nil, fmt.Errorf("stored refresh credential is malformed: json")
	}
	if cred.RefreshToken == "" || cred.TokenEndpoint == "" || cred.Issuer == "" {
		return nil, fmt.Errorf("stored refresh credential is incomplete")
	}
	return &fencedCredential{credential: &cred, generation: generation + 1, previousSlot: slot}, nil
}

// storeFenced persists the refresh credential at a fresh slot and commits
// the fence to it under the caller's live lease epoch. The commit is the
// single decision point and the last fallible step for the slot swap: the
// previous live slot stays referenced until the commit is durable, so a
// rejected or failed commit can never leave the fence pointing at a
// deleted credential. The commit is the credential-side compare-and-swap:
// when a concurrent rotation already passed the generation, or the caller
// lost the lease while its secret-store write was blocked, the commit is
// rejected and the orphan slot is deleted, so a stale writer can never
// make its value live — even before the winner commits its own
// generation. After a durable commit the new slot is the only live
// credential; the replaced credentials — the legacy label and the previous
// live slot — are retired then, the previous slot having been atomically
// enqueued for retirement retry with the commit, so a failed deletion can
// never strand the old grant.
func (c *Client) storeFenced(ctx context.Context, fenced *fencedCredential) error {
	cred := fenced.credential
	data, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("encode refresh credential: %w", err)
	}
	slot, err := newRefreshSlotLabel()
	if err != nil {
		return err
	}
	if err := c.secrets.SetSecret(slot, data); err != nil {
		// A backend may report an uncertain write. Roll back the unique
		// slot; if deletion also fails, keep a durable cleanup record.
		return fmt.Errorf("%w: %w", ErrBackendUnavailable,
			errors.Join(err, c.rollbackRefreshSlot(ctx, slot)))
	}
	c.retireFailedSlots(ctx)
	previous := fenced.previousSlot
	if previous == labelRefresh {
		// The legacy single-label form is a fixed label that every refresh
		// and logout retries; it needs no retirement record.
		previous = ""
	}
	committed, err := c.lease.CommitCredentialFence(ctx, store.CredentialFenceCommit{
		FenceName:       store.RefreshFenceName,
		FenceGeneration: fenced.generation,
		Slot:            slot,
		PreviousSlot:    previous,
		LeaseName:       refreshLeaseName,
		LeaseOwner:      c.owner,
		LeaseGeneration: fenced.leaseGeneration,
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable,
			errors.Join(err, c.rollbackRefreshSlot(ctx, slot)))
	}
	if !committed {
		// A concurrent rotation passed this generation, or this writer
		// lost the lease during the write: the write must not be reported
		// as success and the orphan slot must not linger.
		rejected := fmt.Errorf("%w: refresh credential superseded by a concurrent rotation", ErrBackendUnavailable)
		if rollbackErr := c.rollbackRefreshSlot(ctx, slot); rollbackErr != nil {
			return errors.Join(rejected, rollbackErr)
		}
		return rejected
	}
	// The commit is durable: the new slot is the only live credential and
	// the previous slot is durably enqueued for retirement retry. Retire
	// the replaced credentials and clear the record on success. A new
	// credential also retires any invalidation marker: the fenced slot is
	// now the live grant, and the legacy fallback is out of play until the
	// fence is cleared again. Best effort — a marker left behind can only
	// mask a legacy label this commit just replaced.
	_ = c.lease.ClearRefreshCredentialInvalidation(ctx)
	_ = c.secrets.DeleteSecret(labelRefresh)
	if previous != "" {
		if err := c.secrets.DeleteSecret(previous); err != nil {
			// The record is already durable; the next refresh or logout
			// retries the deletion.
			return nil
		}
		if err := c.lease.ClearRetiredCredentialSlot(ctx, store.RefreshFenceName, previous); err != nil {
			return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
		}
	}
	return nil
}

// rollbackRefreshSlot removes a refresh slot that never became live. A
// secure-backend deletion can fail after the slot write succeeded; record
// that slot durably on a cancellation-independent context so a later
// refresh or logout can retry it instead of orphaning a refresh token.
func (c *Client) rollbackRefreshSlot(ctx context.Context, slot string) error {
	if err := c.secrets.DeleteSecret(slot); err != nil {
		recordErr := c.lease.RecordRetiredCredentialSlot(
			context.WithoutCancel(ctx), store.RefreshFenceName, slot,
		)
		if recordErr != nil {
			return errors.Join(
				fmt.Errorf("delete uncommitted refresh slot: %w", err),
				fmt.Errorf("record uncommitted refresh slot for retirement: %w", recordErr),
			)
		}
		return fmt.Errorf("delete uncommitted refresh slot: %w", err)
	}
	return nil
}

// retireFailedSlots retries the deletion of slots a previous rotation
// failed to retire, clearing their durable records on success. Best
// effort: a slot whose deletion still fails keeps its record for the next
// refresh or logout.
func (c *Client) retireFailedSlots(ctx context.Context) {
	for _, fenceName := range []string{store.RefreshFenceName, store.ClientFenceName} {
		slots, err := c.lease.RetiredCredentialSlots(ctx, fenceName)
		if err != nil {
			continue
		}
		for _, slot := range slots {
			if err := c.secrets.DeleteSecret(slot); err != nil {
				continue
			}
			_ = c.lease.ClearRetiredCredentialSlot(ctx, fenceName, slot)
		}
	}
}
