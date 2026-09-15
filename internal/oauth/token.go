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

// storeRefresh atomically persists the refresh credential.
func (c *Client) storeRefresh(cred *refreshCredential) error {
	data, err := json.Marshal(cred)
	if err != nil {
		return fmt.Errorf("encode refresh credential: %w", err)
	}
	if err := c.secrets.SetSecret(labelRefresh, data); err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	return nil
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

// applyTokens records a successful token exchange: the in-memory access
// token plus, atomically, any replacement refresh token.
func (c *Client) applyTokens(cred *refreshCredential, tok *tokenResponse) error {
	if tok.RefreshToken != "" {
		cred.RefreshToken = tok.RefreshToken
	}
	cred.Updated = c.clock().UTC()
	if err := c.storeRefresh(cred); err != nil {
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
	c.hasToken = true
	return nil
}

// Token returns a currently valid access token, refreshing under the
// profile lease when the held token is missing or within the refresh skew.
// ErrNoCredentials means interactive reauthorization is required; this
// method never opens a browser.
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	valid := c.hasToken && c.tokenExpiry.After(c.clock().Add(refreshSkew))
	if valid {
		defer c.mu.Unlock()
		return c.token, nil
	}
	c.mu.Unlock()
	return c.refresh(ctx)
}

// Expiry returns the held in-memory access token's expiry without
// refreshing. ok is false when no token is held. The subscription owner
// uses this to close a stream no later than credential expiry.
func (c *Client) Expiry() (expiry time.Time, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tokenExpiry, c.hasToken
}

// HasCredentials reports whether a refresh credential is stored for this
// profile.
func (c *Client) HasCredentials() (bool, error) {
	_, found, err := c.loadRefresh()
	return found, err
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
