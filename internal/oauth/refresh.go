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
	// Pre-write gate: the renewal loop keeps the lease alive, and this
	// atomic commit fails when a foreign claim has already taken the
	// ownership epoch captured at claim time, so the stale exchange never
	// reaches the credential write.
	if leaseLost {
		return "", errors.New("refresh lease lost before the credential write")
	}
	if committed, cerr := c.lease.CommitLease(ctx, refreshLeaseName, c.owner, leaseGeneration); ctx.Err() != nil {
		return "", ctx.Err()
	} else if cerr != nil || !committed {
		return "", errors.New("refresh lease lost before the credential write")
	}
	// The fenced store is the persistence fence: the slot write plus the
	// atomic fence commit decide the outcome. The commit is bound to the
	// lease epoch captured at claim time, so a writer whose lease a
	// foreign claim took while its secret-store write was blocked can
	// never advance the fence — even before the winner commits its own
	// generation. A failed commit deletes the orphan slot, and the refresh
	// fails instead of reporting a superseded success.
	if err := c.applyTokens(ctx, fenced, tok, leaseGeneration); err != nil {
		if ctx.Err() == nil {
			c.clearToken()
		}
		return "", err
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
