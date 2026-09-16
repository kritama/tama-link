package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

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

// HasCredentials reports whether the profile can authenticate without
// performing a refresh: a cached token that is not already inside its
// refresh skew, or a stored refresh credential. It is the submit path's
// cheap probe for deciding whether accepting work the profile cannot
// authenticate would only burn the idempotency key on a terminal
// authentication_required. No network I/O.
func (c *Client) HasCredentials() (bool, error) {
	if _, ok := c.validToken(); ok {
		return true, nil
	}
	_, found, err := c.loadRefresh()
	if err != nil {
		return false, err
	}
	return found, nil
}

// applyTokens records a successful token exchange: the in-memory access
// token plus, at the fenced slot the caller derived before the exchange,
// any replacement refresh token.
func (c *Client) applyTokens(ctx context.Context, fenced *fencedCredential, tok *tokenResponse) error {
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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = tok.AccessToken
	c.tokenExpiry = c.clock().Add(time.Duration(expiresIn) * time.Second)
	// The skew is capped at a quarter of the issued lifetime so a short
	// token (for example 60 s) keeps a positive validity window instead
	// of refreshing on every request and rotating the grant away.
	if lifetime := time.Duration(expiresIn) * time.Second; refreshSkew > lifetime/4 {
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
// backend and clears the in-memory token. It never touches the state
// encryption key or the state database.
func (c *Client) Logout() error {
	for _, label := range []string{labelClient, labelRefresh} {
		if err := c.secrets.DeleteSecret(label); err != nil {
			return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
	c.tokenExpiry = time.Time{}
	c.hasToken = false
	return nil
}
