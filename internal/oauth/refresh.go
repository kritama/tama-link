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
	if err := checkStoredTokenEndpoint(cred.TokenEndpoint, cred.Issuer); err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoCredentials, err)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cred.RefreshToken)
	form.Set("resource", c.endpoint)
	tok, err := c.postToken(ctx, cred.TokenEndpoint, rec, form)
	if err != nil {
		if errors.Is(err, ErrGrantInvalid) {
			c.clearToken()
		}
		return "", err
	}
	if err := c.applyTokens(cred, tok); err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// checkStoredTokenEndpoint validates the persisted token endpoint against
// the issuer-bound metadata policy: a secure absolute URL on the same
// origin as the issuer it claims.
func checkStoredTokenEndpoint(endpoint, issuer string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("stored token endpoint is not an absolute URL")
	}
	if err := checkSecureURL(u); err != nil {
		return fmt.Errorf("stored token endpoint: %w", err)
	}
	if endpointOrigin(endpoint) != endpointOrigin(issuer) {
		return fmt.Errorf("stored token endpoint origin does not match its issuer")
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
