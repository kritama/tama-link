package oauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	generation, slot, found, err := c.lease.ReadCredentialFence(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	if !found {
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
// the fence to it under the caller's live lease epoch. Retirement of the
// credentials this commit replaces — the legacy single-label form and the
// previous live slot — happens BEFORE the commit: the fence commit is the
// single decision point, so a retirement failure aborts with rollback of
// the new slot while the still-referenced previous slot remains
// discoverable and is retried by the next refresh. After the commit the
// previous slot would be undiscoverable, and a failure could silently
// strand a still-valid grant in the backend. The commit itself is the
// credential-side compare-and-swap: when a concurrent rotation already
// passed the generation, or the caller lost the lease while its
// secret-store write was blocked, the commit is rejected and the orphan
// slot is deleted, so a stale writer can never make its value live — even
// before the winner commits its own generation. A successful commit
// leaves the new slot as the only live credential.
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
	rollback := func(reason error) error {
		_ = c.secrets.DeleteSecret(slot)
		return reason
	}
	if err := c.secrets.SetSecret(slot, data); err != nil {
		// Best-effort rollback of the uncommitted slot.
		return rollback(fmt.Errorf("%w: %w", ErrBackendUnavailable, err))
	}
	if err := c.secrets.DeleteSecret(labelRefresh); err != nil {
		return rollback(fmt.Errorf("%w: %w", ErrBackendUnavailable, err))
	}
	if previous := fenced.previousSlot; previous != "" && previous != labelRefresh {
		if err := c.secrets.DeleteSecret(previous); err != nil {
			return rollback(fmt.Errorf("%w: %w", ErrBackendUnavailable, err))
		}
	}
	committed, err := c.lease.CommitCredentialFence(
		ctx, fenced.generation, slot, refreshLeaseName, c.owner, fenced.leaseGeneration)
	if err != nil {
		return rollback(fmt.Errorf("%w: %w", ErrBackendUnavailable, err))
	}
	if !committed {
		// A concurrent rotation passed this generation, or this writer
		// lost the lease during the write: the write must not be reported
		// as success and the orphan slot must not linger.
		return rollback(fmt.Errorf("%w: refresh credential superseded by a concurrent rotation", ErrBackendUnavailable))
	}
	return nil
}
