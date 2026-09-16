package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// claimAttempts bounds how long one process waits for the refresh lease
// before failing. Waiting is bounded; there is no unbounded retry loop.
const claimAttempts = 3

// claimInterval is the pause between lease claim attempts.
const claimInterval = 250 * time.Millisecond

// refreshLocked performs the lease-coordinated refresh transaction:
//
//  1. load the stored client record and refresh credential;
//  2. claim the profile refresh lease (bounded attempts);
//  3. re-read the refresh credential after claiming, so a replacement
//     written by another process is adopted before the exchange;
//  4. exchange the refresh grant;
//  5. atomically retain any replacement refresh token.
//
// refreshLocked performs one refresh transaction. The caller holds
// refreshMu for the whole call, which is what makes the in-process
// serialization hold: the cross-process lease is shared by every refresh
// in this process (ClaimLease treats a lease already held by this owner as
// acquired), so without the mutex concurrent in-process refreshes would
// rotate the same refresh grant at once.
func (c *Client) refreshLocked(ctx context.Context) (string, error) {
	rec, found, err := c.RegisteredClient()
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: no client registration", ErrNoCredentials)
	}
	if _, err := c.loadFenced(ctx); err != nil {
		return "", err
	}

	leaseGeneration, claimed, err := c.claimRefreshLease(ctx)
	if err != nil {
		return "", err
	}
	if !claimed {
		return "", fmt.Errorf("%w: the winning process is refreshing the credential", ErrLeaseContention)
	}
	defer func() { _ = c.lease.ReleaseLease(context.WithoutCancel(ctx), refreshLeaseName, c.owner) }()

	fenced, err := c.loadFenced(ctx)
	if err != nil {
		return "", err
	}
	if fenced == nil {
		return "", fmt.Errorf("%w: no refresh credential", ErrNoCredentials)
	}
	cred := fenced.credential
	// Both stored values must bind the active profile issuer: if the profile
	// changed issuer while retaining its credential namespace, refresh fails
	// closed instead of replaying a foreign token to a stored endpoint.
	if rec.Issuer != c.issuer {
		return "", fmt.Errorf("%w: stored client registration binds a different issuer than the active profile", ErrNoCredentials)
	}
	if cred.Issuer != c.issuer {
		return "", fmt.Errorf("%w: stored refresh credential binds a different issuer than the active profile", ErrNoCredentials)
	}
	if rec.Issuer != cred.Issuer {
		return "", fmt.Errorf("%w: client registration and refresh credential bind different issuers", ErrNoCredentials)
	}
	if err := checkIssuerBoundEndpoint(cred.TokenEndpoint, cred.Issuer); err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoCredentials, err)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cred.RefreshToken)
	form.Set("resource", c.endpoint)

	// The exchange is a network call and the credential write follows it,
	// so the lease is renewed on a third of its TTL across the whole
	// critical section — the exchange and the fenced persistence — and
	// ownership is verified before the write.
	tok, err := c.leasedExchangeAndPersist(ctx, leaseGeneration,
		func(ectx context.Context) (*tokenResponse, error) {
			return c.postToken(ectx, cred.TokenEndpoint, rec, form)
		},
		func(pctx context.Context, tok *tokenResponse) error {
			// The fenced store is the persistence fence: the slot write
			// plus the atomic fence commit decide the outcome. The commit
			// is bound to the lease epoch captured at claim time, so a
			// writer whose lease a foreign claim took can never advance
			// the fence. A failed commit deletes the orphan slot, and the
			// refresh fails instead of reporting a superseded success.
			return c.applyTokens(pctx, fenced, tok, leaseGeneration)
		})
	if err != nil {
		if errors.Is(err, ErrGrantInvalid) {
			c.clearToken()
			// The grant is known-invalid: invalidate the durable refresh
			// credential too, so readiness rejects new work as
			// authentication_required instead of accepting submissions
			// that can only fail on the same grant. The caller holds the
			// lease epoch, so the fence clear is gated on it.
			if invalidateErr := c.invalidateCredential(ctx, leaseGeneration); invalidateErr != nil {
				return "", errors.Join(err, invalidateErr)
			}
		}
		return "", err
	}
	return tok.AccessToken, nil
}

// invalidateCredential removes the durable refresh credential after the
// authorization server rejected the grant with invalid_grant: the fence
// pointer and the fenced slot, plus the legacy label. The caller holds the
// refresh lease (epoch leaseGeneration). The pointer and the slot's
// retirement record are committed in one transaction — under a renewal
// span, honoring a lost epoch — before the slot is deleted, so a crash or
// a later deletion failure can never leave the slot with no durable
// reference; a failed deletion keeps the record for the next refresh or
// logout, and the record is removed only after the deletion succeeds. The
// durable invalidation marker is established before the fence is
// cleared, so the rejected grant cannot be resurrected through the
// fence-less legacy fallback by a legacy label that survives the fence
// being the active reference, and the marker is cleared only once the
// label is gone. Without
// invalidation, HasCredentials would keep reporting the profile ready and
// every submit would burn an idempotency key on a terminal failure against
// the same known-invalid grant.
func (c *Client) invalidateCredential(ctx context.Context, leaseGeneration int64) error {
	// Renew ownership across the cleanup: the failed exchange's renewal
	// loop has already stopped, and a slow secure-backend deletion must
	// not outlive the lease TTL.
	execCtx, cancelExec := context.WithCancel(ctx)
	renewed := make(chan struct{})
	go c.renewLease(execCtx, cancelExec, renewed)
	defer func() {
		cancelExec()
		<-renewed
	}()

	fenced, err := c.loadFenced(execCtx)
	if err != nil {
		return err
	}
	// The fenced slot is enqueued for retirement retry in the same
	// transaction as the clear, so the pointer never leaves the store
	// without a durable reference to its slot. The legacy label is a
	// fixed label every logout retries; it needs no record.
	slot := ""
	if fenced != nil && fenced.previousSlot != labelRefresh {
		slot = fenced.previousSlot
	}
	// Clearing the fence makes a surviving legacy label eligible for the
	// fence-less fallback, so the invalidation marker is established
	// before the clear: no window exists — including a crash or a failed
	// fenced-slot deletion — in which the rejected grant is readable
	// again.
	if err := c.lease.MarkRefreshCredentialInvalidated(execCtx); err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	cleared, err := c.lease.ClearCredentialFence(execCtx, refreshLeaseName, c.owner, leaseGeneration, slot)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	if !cleared {
		return fmt.Errorf("%w: refresh lease lost while invalidating the rejected grant; retry", ErrLeaseContention)
	}
	if slot != "" {
		// The record is durable: deleting now is cleanup of a rejected
		// grant, so a failed deletion keeps the record and the refresh
		// outcome unchanged.
		if err := c.secrets.DeleteSecret(slot); err != nil {
			return nil
		}
		if err := c.lease.ClearRetiredCredentialSlot(execCtx, slot); err != nil {
			return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
		}
	}
	// The legacy label holds the rejected grant: the marker is already
	// durable, so a failed deletion cannot bring the grant back to life
	// through the fence-less legacy fallback. The marker is cleared only
	// once the deletion succeeds; the mark itself is the retryable
	// record, and every later refresh and logout keeps the grant masked.
	if err := c.secrets.DeleteSecret(labelRefresh); err != nil {
		return nil
	}
	if err := c.lease.ClearRefreshCredentialInvalidation(execCtx); err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	return nil
}

// retireOrphanedCredential removes a durable refresh credential left over
// from a previous client registration. A replacement registration issues
// a new client ID: every stored refresh grant is bound to the old client
// and can never refresh under the new one, so readiness must not pair
// them. The standard invalidation sequence runs under a freshly claimed
// refresh lease. When no credential is stored — the normal first-login
// path — this is a no-op.
func (c *Client) retireOrphanedCredential(ctx context.Context) error {
	if existing, err := c.loadFenced(ctx); err != nil {
		return err
	} else if existing == nil {
		return nil
	}
	leaseGeneration, claimed, err := c.claimRefreshLease(ctx)
	if err != nil {
		return err
	}
	if !claimed {
		return fmt.Errorf("%w: the winning process is refreshing the credential", ErrLeaseContention)
	}
	defer func() { _ = c.lease.ReleaseLease(context.WithoutCancel(ctx), refreshLeaseName, c.owner) }()
	return c.invalidateCredential(ctx, leaseGeneration)
}

// leasedExchangeAndPersist renews the claimed refresh lease on a third of
// its TTL across one token exchange AND the credential persistence that
// follows it: stopping the renewal between them would let a slow
// secret-store write outlive the lease TTL and reject the replacement
// after the endpoint already rotated the grant. Ownership is verified
// immediately before persistence with an atomic commit gate on the
// captured epoch; a renewal that fails or reports lost ownership cancels
// the working context so the stale write aborts. The caller has claimed
// the lease (epoch leaseGeneration) and owns its release. A plain caller
// cancellation surfaces the exchange error unchanged; the renewal loop
// only cancels its own context.
func (c *Client) leasedExchangeAndPersist(
	ctx context.Context,
	leaseGeneration int64,
	exchange func(context.Context) (*tokenResponse, error),
	persist func(context.Context, *tokenResponse) error,
) (*tokenResponse, error) {
	execCtx, cancelExec := context.WithCancel(ctx)
	renewed := make(chan struct{})
	go c.renewLease(execCtx, cancelExec, renewed)
	defer func() {
		cancelExec()
		<-renewed
	}()
	tok, err := exchange(execCtx)
	leaseLost := false
	select {
	case <-execCtx.Done():
		// The renewal loop only cancels execCtx when the lease was lost; a
		// plain caller cancellation leaves the exchange error intact.
		leaseLost = ctx.Err() == nil
	default:
	}
	if err != nil {
		if leaseLost {
			return nil, fmt.Errorf("refresh lease lost during the token exchange: %w", err)
		}
		return nil, err
	}
	if leaseLost {
		return nil, errors.New("refresh lease lost before the credential write")
	}
	if committed, cerr := c.lease.CommitLease(ctx, refreshLeaseName, c.owner, leaseGeneration); ctx.Err() != nil {
		return nil, ctx.Err()
	} else if cerr != nil || !committed {
		return nil, errors.New("refresh lease lost before the credential write")
	}
	if err := persist(execCtx, tok); err != nil {
		return nil, err
	}
	return tok, nil
}

// renewLease renews the refresh lease every third of its TTL until the
// caller's context is cancelled. A renewal that fails or reports lost
// ownership cancels the exchange context so the caller aborts before
// persisting a replacement credential.
func (c *Client) renewLease(ctx context.Context, cancel context.CancelFunc, done chan<- struct{}) {
	defer close(done)
	interval := refreshLeaseTTL / 3
	if interval <= 0 {
		interval = refreshLeaseTTL
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			renewCtx, cancelRenew := context.WithTimeout(ctx, interval)
			owned, err := c.lease.RenewLease(renewCtx, refreshLeaseName, c.owner, refreshLeaseTTL)
			cancelRenew()
			if ctx.Err() != nil {
				return
			}
			if err != nil || !owned {
				cancel()
				return
			}
			timer.Reset(interval)
		}
	}
}

// checkIssuerBoundEndpoint validates one token endpoint against the
// issuer-bound policy: a secure absolute URL on the same origin as the
// issuer it claims. The authorization code and any client secret are sent
// to this endpoint, so an unrelated origin is rejected both when fresh
// metadata is validated at discovery and when a stored credential is
// checked before refresh.
func checkIssuerBoundEndpoint(endpoint, issuer string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("token endpoint is not an absolute URL")
	}
	if err := checkSecureURL(u); err != nil {
		return err
	}
	if endpointOrigin(endpoint) != endpointOrigin(issuer) {
		return fmt.Errorf("token endpoint origin does not match the issuer")
	}
	return nil
}

// claimRefreshLease claims the refresh lease with bounded attempts and
// returns the claimed ownership epoch.
func (c *Client) claimRefreshLease(ctx context.Context) (int64, bool, error) {
	for attempt := 0; attempt < claimAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return 0, false, ctx.Err()
			case <-time.After(claimInterval):
			}
		}
		claimed, err := c.lease.ClaimLease(ctx, refreshLeaseName, c.owner, refreshLeaseTTL)
		if err != nil {
			return 0, false, fmt.Errorf("claim refresh lease: %w", err)
		}
		if claimed {
			generation, ok, err := c.lease.LeaseGeneration(ctx, refreshLeaseName, c.owner)
			if err != nil {
				return 0, false, err
			}
			if !ok {
				return 0, false, errors.New("refresh lease lost before the credential write")
			}
			return generation, true, nil
		}
	}
	return 0, false, nil
}

// clearToken drops the in-memory access token after a grant failure.
func (c *Client) clearToken() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
	c.tokenExpiry = time.Time{}
	c.hasToken = false
}
