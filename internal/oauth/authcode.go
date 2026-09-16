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
	// RedirectURI is the exact loopback URI in the authorization request.
	// The listener that serves it must observe exactly this URI; the token
	// exchange resends it verbatim because the authorization-code grant
	// requires both values to be equal.
	RedirectURI string
}

// NewAuthorizationRequest builds one authorization URL with PKCE S256 and
// RFC 8707 resource binding. redirectURI must be the exact URI the loopback
// listener will serve: the ephemeral listener port must be selected before
// this is called, and that exact URI is what the authorization request and
// the later token exchange both carry.
func (c *Client) NewAuthorizationRequest(md *Metadata, rec *ClientRecord, redirectURI string) (*AuthorizationRequest, error) {
	if rec.Issuer != md.AS.Issuer {
		return nil, fmt.Errorf("%w: client registration binds a different issuer", ErrMetadata)
	}
	if err := checkLoopbackRedirect(redirectURI); err != nil {
		return nil, err
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
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("resource", c.endpoint)
	endpoint.RawQuery = q.Encode()
	return &AuthorizationRequest{URL: endpoint, State: state, Verifier: verifier, RedirectURI: redirectURI}, nil
}

// CompleteAuthorization exchanges an authorization code for tokens. The
// authorization-code grant requires the token request's redirect_uri to
// equal the value sent in the authorization request, so observedRedirectURI
// must equal req.RedirectURI exactly; a mismatch is rejected before any
// token request is sent. The exchanged access token is held in memory; the
// refresh credential is persisted.
func (c *Client) CompleteAuthorization(ctx context.Context, md *Metadata, rec *ClientRecord, req *AuthorizationRequest, code, observedRedirectURI string) error {
	if code == "" {
		return fmt.Errorf("authorization code is required")
	}
	if req == nil {
		return fmt.Errorf("authorization request is required")
	}
	if observedRedirectURI != req.RedirectURI {
		return fmt.Errorf("observed redirect uri does not match the authorization request")
	}
	if err := checkLoopbackRedirect(req.RedirectURI); err != nil {
		return err
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", req.RedirectURI)
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
	// The authorization commits its credential through the same fence as
	// every refresh, so a concurrent rotation is detected by the commit,
	// never clobbered.
	fenced, err := c.loadFenced(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	if fenced == nil {
		fenced = &fencedCredential{generation: 1}
	}
	fenced.credential = cred
	return c.applyTokens(ctx, fenced, tok)
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
