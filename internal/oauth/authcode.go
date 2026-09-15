package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
)

// AuthorizationRequest is one pending authorization-code attempt. The
// verifier must be held in memory by the caller until the exchange; it is
// never persisted.
type AuthorizationRequest struct {
	// URL is the authorization endpoint URL to present to the user.
	URL *url.URL
	// State is the opaque anti-CSRF value to compare on redirect.
	State string
	// Verifier is the PKCE code verifier required by the exchange.
	Verifier string
	// RedirectURI is the registered loopback redirect URI.
	RedirectURI string
}

// NewAuthorizationRequest builds one authorization URL with PKCE S256 and
// RFC 8707 resource binding.
func (c *Client) NewAuthorizationRequest(md *Metadata, rec *ClientRecord) (*AuthorizationRequest, error) {
	if rec.Issuer != md.AS.Issuer {
		return nil, fmt.Errorf("%w: client registration binds a different issuer", ErrMetadata)
	}
	verifier, err := randomToken(48)
	if err != nil {
		return nil, err
	}
	state, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	endpoint, err := url.Parse(md.AS.AuthorizationEndpoint)
	if err != nil {
		return nil, fmt.Errorf("parse authorization endpoint: %w", err)
	}
	q := endpoint.Query()
	q.Set("response_type", "code")
	q.Set("client_id", rec.ClientID)
	q.Set("redirect_uri", c.redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("resource", c.endpoint)
	endpoint.RawQuery = q.Encode()
	return &AuthorizationRequest{URL: endpoint, State: state, Verifier: verifier, RedirectURI: c.redirectURI}, nil
}

// CompleteAuthorization exchanges an authorization code for tokens, using
// the loopback redirect URI actually observed by the listener. The exchanged
// access token is held in memory; the refresh credential is persisted.
func (c *Client) CompleteAuthorization(ctx context.Context, md *Metadata, rec *ClientRecord, req *AuthorizationRequest, code, observedRedirectURI string) error {
	if code == "" {
		return fmt.Errorf("authorization code is required")
	}
	if err := checkLoopbackRedirect(observedRedirectURI); err != nil {
		return err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", observedRedirectURI)
	form.Set("code_verifier", req.Verifier)
	form.Set("resource", c.endpoint)
	tok, err := c.postToken(ctx, md.AS.TokenEndpoint, rec, form)
	if err != nil {
		return err
	}
	if tok.RefreshToken == "" {
		return fmt.Errorf("%w: authorization code exchange returned no refresh token", ErrNoCredentials)
	}
	cred := &refreshCredential{
		RefreshToken:  tok.RefreshToken,
		TokenEndpoint: md.AS.TokenEndpoint,
		Issuer:        md.AS.Issuer,
		Updated:       c.clock().UTC(),
	}
	return c.applyTokens(cred, tok)
}

// checkLoopbackRedirect enforces D5: the only redirect this client accepts
// is an http 127.0.0.1 loopback URI.
func checkLoopbackRedirect(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		return fmt.Errorf("redirect uri must be an http 127.0.0.1 loopback uri")
	}
	return nil
}

// randomToken returns n random bytes base64url-encoded without padding.
func randomToken(n int) (string, error) {
	var b [64]byte
	if n > len(b) {
		return "", fmt.Errorf("token too large")
	}
	if _, err := rand.Read(b[:n]); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:n]), nil
}
