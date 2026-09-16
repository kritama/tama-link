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

// Refresh forces one refresh exchange under the profile lease and returns
// the new access token. The subscription owner calls it after closing a
// stream at credential expiry and before reconciling through tasks/get.
func (c *Client) Refresh(ctx context.Context) (string, error) {
	return c.refresh(ctx)
}

// refresh performs the lease-coordinated refresh transaction:
//
//  1. load the stored client record and refresh credential;
//  2. claim the profile refresh lease (bounded attempts);
//  3. re-read the refresh credential after claiming, so a replacement
//     written by another process is adopted before the exchange;
//  4. exchange the refresh grant;
//  5. atomically retain any replacement refresh token.
func (c *Client) refresh(ctx context.Context) (string, error) {
	rec, found, err := c.RegisteredClient()
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: no client registration", ErrNoCredentials)
	}
	if _, found, err := c.loadRefresh(); err != nil {
		return "", err
	} else if !found {
		return "", fmt.Errorf("%w: no refresh credential", ErrNoCredentials)
	}

	// In-process serialization: the cross-process lease is shared by every
	// refresh in this process (ClaimLease treats a lease already held by
	// this owner as acquired), so concurrent in-process refreshes would
	// otherwise rotate the same refresh grant at once. The mutex makes the
	// whole claim-through-release section one-at-a-time per process.
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	claimed, err := c.claimRefreshLease(ctx)
	if err != nil {
		return "", err
	}
	if !claimed {
		return "", errors.New("oauth: refresh lease is held by another process")
	}
	defer func() { _ = c.lease.ReleaseLease(context.WithoutCancel(ctx), refreshLeaseName, c.owner) }()

	cred, found, err := c.loadRefresh()
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%w: no refresh credential", ErrNoCredentials)
	}
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
	// so the lease is renewed on a third of its TTL for the whole critical
	// section, including the write. Losing the lease aborts before any
	// replacement token is persisted: another process is now responsible
	// for the credential, and a double rotation would invalidate its
	// replacement.
	execCtx, cancelExec := context.WithCancel(ctx)
	renewed := make(chan struct{})
	go c.renewLease(execCtx, cancelExec, renewed)
	defer func() {
		cancelExec()
		<-renewed
	}()
	tok, err := c.postToken(execCtx, cred.TokenEndpoint, rec, form)
	leaseLost := false
	select {
	case <-execCtx.Done():
		// The renewal loop only cancels execCtx when the lease was lost; a
		// plain caller cancellation leaves the exchange error intact.
		leaseLost = ctx.Err() == nil
	default:
	}
	if err != nil {
		if errors.Is(err, ErrGrantInvalid) {
			c.clearToken()
		}
		if leaseLost {
			return "", fmt.Errorf("refresh lease lost during the token exchange: %w", err)
		}
		return "", err
	}
	// Ownership is re-verified immediately before the write, and the
	// renewal loop keeps running through it, so a blocked credential
	// backend cannot outlast the lease without the loss cancelling this
	// path. A lost verification aborts before SetSecret runs.
	if leaseLost {
		return "", errors.New("refresh lease lost before the credential write")
	}
	renewCtx, cancelRenew := context.WithTimeout(ctx, refreshLeaseTTL/2)
	owned, err := c.lease.RenewLease(renewCtx, refreshLeaseName, c.owner, refreshLeaseTTL)
	cancelRenew()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || !owned {
		return "", errors.New("refresh lease lost before the credential write")
	}
	if err := c.applyTokens(cred, tok); err != nil {
		return "", err
	}
	// The write cannot observe the renewal loop: the secret-store API has
	// no context, so a blocked write can outlive the lease and a loss
	// cannot interrupt it. Re-verify ownership after the write; if it was
	// lost, another process now owns the credential and this result must
	// not be reported as success or cached, even though the write itself
	// already ran.
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if execCtx.Err() != nil {
		c.clearToken()
		return "", errors.New("refresh lease lost during the credential write")
	}
	renewCtx, cancelRenew = context.WithTimeout(ctx, refreshLeaseTTL/2)
	owned, err = c.lease.RenewLease(renewCtx, refreshLeaseName, c.owner, refreshLeaseTTL)
	cancelRenew()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || !owned {
		c.clearToken()
		return "", errors.New("refresh lease lost during the credential write")
	}
	return tok.AccessToken, nil
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

// claimRefreshLease claims the refresh lease with bounded attempts.
func (c *Client) claimRefreshLease(ctx context.Context) (bool, error) {
	for attempt := 0; attempt < claimAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(claimInterval):
			}
		}
		claimed, err := c.lease.ClaimLease(ctx, refreshLeaseName, c.owner, refreshLeaseTTL)
		if err != nil {
			return false, fmt.Errorf("claim refresh lease: %w", err)
		}
		if claimed {
			return true, nil
		}
	}
	return false, nil
}

// clearToken drops the in-memory access token after a grant failure.
func (c *Client) clearToken() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
	c.tokenExpiry = time.Time{}
	c.hasToken = false
}
