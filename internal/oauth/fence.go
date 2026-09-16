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

// fencedCredential is one refresh credential plus the generation and slot
// a replacement must be committed at.
type fencedCredential struct {
	credential *refreshCredential
	generation int64
}

// loadFenced loads the live refresh credential. When no fence has been
// committed yet, the legacy single-label credential is adopted as
// generation 1. The returned generation is what a replacement commits at.
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
		return &fencedCredential{credential: cred, generation: 1}, nil
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
	return &fencedCredential{credential: &cred, generation: generation + 1}, nil
}

// storeFenced persists the refresh credential at a fresh slot and commits
// the fence to it. The commit is the credential-side compare-and-swap:
// when a concurrent rotation already passed the generation, the commit is
// rejected and the orphan slot is deleted, so a stale writer can never
// make its value live — even if its secret-store write blocks past the
// lease TTL. A successful commit leaves the new slot as the only live
// credential and retires the legacy label.
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
		// Best-effort rollback of the uncommitted slot.
		_ = c.secrets.DeleteSecret(slot)
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	committed, err := c.lease.CommitCredentialFence(ctx, fenced.generation, slot)
	if err != nil {
		_ = c.secrets.DeleteSecret(slot)
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	if !committed {
		// A concurrent rotation passed this generation: the write must
		// not be reported as success and the orphan slot must not linger.
		_ = c.secrets.DeleteSecret(slot)
		return fmt.Errorf("%w: refresh credential superseded by a concurrent rotation", ErrBackendUnavailable)
	}
	_ = c.secrets.DeleteSecret(labelRefresh)
	return nil
}
