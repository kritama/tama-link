package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/kritama/tama-link/internal/store"
)

// maxTokenExpirySeconds is the largest provider-reported expires_in this
// client converts to a duration: the seconds representable by time.Duration.
// Larger values would wrap negative on the nanosecond multiplication and
// commit a credential that is immediately expired.
const maxTokenExpirySeconds = int64(math.MaxInt64) / int64(time.Second)

// refreshCredential is the persisted refresh grant material. It names the
// token endpoint and issuer it was issued from so refresh never needs
// rediscovery, and so a profile that points at a different authorization
// server fails closed instead of replaying a foreign credential.
type refreshCredential struct {
	RefreshToken  string    `json:"refresh_token"`
	TokenEndpoint string    `json:"token_endpoint"`
	Issuer        string    `json:"issuer"`
	Updated       time.Time `json:"updated"`
}

// loadRefresh returns the stored refresh credential, or found=false.
func (c *Client) loadRefresh() (*refreshCredential, bool, error) {
	data, found, err := c.secrets.GetSecret(labelRefresh)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	if !found {
		return nil, false, nil
	}
	var cred refreshCredential
	if err := json.Unmarshal(data, &cred); err != nil {
		return nil, false, fmt.Errorf("stored refresh credential is malformed: json")
	}
	if cred.RefreshToken == "" || cred.TokenEndpoint == "" || cred.Issuer == "" {
		return nil, false, fmt.Errorf("stored refresh credential is incomplete")
	}
	return &cred, true, nil
}

// HasCredentials reports whether the profile can authenticate: the
// complete usable client/refresh pair a refresh would find. It is the
// submit path's cheap probe for deciding whether accepting work the
// profile cannot authenticate would only burn the idempotency key on a
// terminal authentication_required. No network I/O. The probe resolves
// the same fenced slot Token loads, so a credential stored by any process
// — legacy label or committed fence — counts. A cached in-memory token
// does not by itself count: another process may have logged the profile
// out, and a cached token would keep accepting work that the worker can
// only terminate as authentication_required. The probe applies the same
// issuer and endpoint bindings refreshLocked enforces: a credential
// refresh could not use — a missing client record, or a record or
// credential no longer bound to the active profile issuer or its token
// endpoint — is not ready, so submit keeps the idempotency key free for
// the reauthorized retry instead of accepting a doomed row.
func (c *Client) HasCredentials(ctx context.Context) (bool, error) {
	rec, found, err := c.RegisteredClient(ctx)
	if err != nil {
		return false, err
	}
	fenced, err := c.loadFenced(ctx)
	if err != nil {
		return false, err
	}
	if !found || fenced == nil {
		return false, nil
	}
	if rec.Issuer != c.issuer || fenced.credential.Issuer != c.issuer {
		return false, nil
	}
	if err := checkIssuerBoundEndpoint(fenced.credential.TokenEndpoint, fenced.credential.Issuer); err != nil {
		return false, nil
	}
	return true, nil
}

// applyTokens records a successful token exchange: the in-memory access
// token plus, at the fenced slot the caller derived before the exchange,
// any replacement refresh token. The fence commit is bound to the lease
// epoch the caller captured at claim time.
func (c *Client) applyTokens(ctx context.Context, fenced *fencedCredential, tok *tokenResponse, leaseGeneration int64) error {
	fenced.leaseGeneration = leaseGeneration
	cred := fenced.credential
	if tok.RefreshToken != "" {
		cred.RefreshToken = tok.RefreshToken
	}
	cred.Updated = c.clock().UTC()
	if err := c.storeFenced(ctx, fenced); err != nil {
		return err
	}
	expiresIn := tok.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 300
	}
	// A provider-reported lifetime beyond the largest value time.Duration
	// can represent would wrap negative on the nanosecond conversion and
	// commit a credential already expired; cap it before converting.
	if expiresIn > maxTokenExpirySeconds {
		expiresIn = maxTokenExpirySeconds
	}
	lifetime := time.Duration(expiresIn) * time.Second
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = tok.AccessToken
	c.tokenExpiry = c.clock().Add(lifetime)
	// The skew is capped at a quarter of the issued lifetime so a short
	// token (for example 60 s) keeps a positive validity window instead
	// of refreshing on every request and rotating the grant away.
	if refreshSkew > lifetime/4 {
		c.skew = lifetime / 4
	} else {
		c.skew = refreshSkew
	}
	c.hasToken = true
	return nil
}

// Token returns a currently valid access token, refreshing under the
// profile lease when the held token is missing or within the refresh skew.
// ErrNoCredentials means interactive reauthorization is required; this
// method never opens a browser.
func (c *Client) Token(ctx context.Context) (string, error) {
	if tok, ok := c.validToken(); ok {
		return tok, nil
	}
	// Coalesced refresh: take the single-flight lock, then recheck. A
	// caller ahead in the queue may have installed a fresh token while we
	// waited, and reusing it spares a sequential rotation per caller in a
	// burst of concurrent expiries.
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if tok, ok := c.validToken(); ok {
		return tok, nil
	}
	return c.refreshLocked(ctx)
}

func (c *Client) validToken() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasToken && c.tokenExpiry.After(c.clock().Add(c.skew)) {
		return c.token, true
	}
	return "", false
}

// Refresh forces one refresh exchange under the profile lease and returns
// the new access token, even when a valid token is cached. It never
// coalesces with a concurrent refresh's result. The subscription owner
// calls it after closing a stream at credential expiry.
func (c *Client) Refresh(ctx context.Context) (string, error) {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	return c.refreshLocked(ctx)
}

// Expiry returns the held in-memory access token's expiry without
// refreshing. ok is false when no token is held. The subscription owner
// uses this to close a stream no later than credential expiry.
func (c *Client) Expiry() (expiry time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokenExpiry, c.hasToken
}

// Logout removes every OAuth credential for the profile from the secure
// backend and clears the in-memory token. It holds the local refresh lock
// and claims the cross-process refresh lease before touching anything: a
// credential writer commits its fence and slot only under a live lease
// epoch, so the claim makes every in-flight writer's commit fail and roll
// back its own slot, and no writer can reinstall a credential behind the
// logout. A contended claim fails with ErrLeaseContention so the caller
// can retry once the other process's refresh completes. Ownership is
// renewed through the whole cleanup: a deletion that blocks past the TTL
// must not let another process take over and reinstall credentials behind
// a half-finished logout, and the fence clear is bound to the claimed
// epoch, so a lost lease can never wipe a newer fence. The committed slot
// is deleted while the fence still references it: a failed deletion keeps
// the slot discoverable, so a retried logout finishes the cleanup. The
// fence is cleared once its slot is gone, then the legacy labels and any
// retirement backlog are deleted; a later login starts from a clean
// fence. Logout never touches the state encryption key or the state
// database.
func (c *Client) Logout(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	leaseGeneration, claimed, err := c.claimRefreshLease(ctx)
	if err != nil {
		return err
	}
	if !claimed {
		return fmt.Errorf("%w: another process is refreshing the credential; retry logout", ErrLeaseContention)
	}
	defer func() { _ = c.lease.ReleaseLease(context.WithoutCancel(ctx), refreshLeaseName, c.owner) }()

	// Renew ownership through the whole cleanup: a slow secure-backend
	// deletion must not outlive the lease TTL and hand the profile to
	// another process mid-logout. The renewal runs on a context that
	// outlives the caller's cancellation because the fixed-label deletes
	// take no context and cannot be aborted; a cancel mid-delete must
	// not hand the epoch away while the stale deletion could still land
	// on a new registration. The interruptible steps still observe the
	// caller context directly.
	renewCtx, cancelExec := context.WithCancel(context.WithoutCancel(ctx))
	renewed := make(chan struct{})
	go c.renewLease(renewCtx, cancelExec, renewed)
	defer func() {
		cancelExec()
		<-renewed
	}()

	_, slot, found, err := c.lease.ReadCredentialFence(ctx, store.RefreshFenceName)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	if found && slot != "" {
		if err := c.secrets.DeleteSecret(slot); err != nil {
			return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
		}
	}
	// Clearing the fence makes a surviving legacy label eligible for the
	// fence-less fallback, so the durable invalidation marker is
	// established before the clear; it is cleared once the label is gone.
	if err := c.lease.MarkRefreshCredentialInvalidated(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	cleared, err := c.lease.ClearCredentialFence(ctx, store.RefreshFenceName, refreshLeaseName, c.owner, leaseGeneration, "")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	if !cleared {
		return fmt.Errorf("%w: logout lost the refresh lease mid-cleanup; retry", ErrLeaseContention)
	}
	// The client registration fence follows the same protocol: the
	// committed slot is deleted while the fence still references it, and
	// the pointer is cleared against the claimed epoch, so a logout whose
	// lease was lost mid-cleanup can never wipe a registration installed
	// by the process that took over.
	if _, clientSlot, clientFound, err := c.lease.ReadCredentialFence(ctx, store.ClientFenceName); err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	} else if clientFound && clientSlot != "" {
		if err := c.secrets.DeleteSecret(clientSlot); err != nil {
			return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
		}
	}
	if cleared, err := c.lease.ClearCredentialFence(ctx, store.ClientFenceName, refreshLeaseName, c.owner, leaseGeneration, ""); err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	} else if !cleared {
		return fmt.Errorf("%w: logout lost the refresh lease mid-cleanup; retry", ErrLeaseContention)
	}
	// The legacy single-label record is only live when no client fence
	// exists; with a fence it is dead data, so the fixed-label delete is
	// harmless either way.
	if err := c.secrets.DeleteSecret(labelClient); err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	if err := c.secrets.DeleteSecret(labelRefresh); err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	_ = c.lease.ClearRefreshCredentialInvalidation(ctx)
	// Drain the retirement backlog: slots a failed rotation left behind.
	// Best effort — a slot whose deletion still fails keeps its durable
	// record for the next refresh.
	c.retireFailedSlots(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
	c.tokenExpiry = time.Time{}
	c.hasToken = false
	return nil
}
