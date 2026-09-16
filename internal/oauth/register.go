package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Secret labels inside the profile credential namespace.
const (
	labelClient  = "oauth-client"
	labelRefresh = "oauth-refresh"
)

// ClientRecord is the persisted dynamic client registration. The secret,
// when present, lives only in the credential backend.
type ClientRecord struct {
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret,omitempty"`
	AuthMethod   string    `json:"token_endpoint_auth_method"`
	Issuer       string    `json:"issuer"`
	RegisteredAt time.Time `json:"registered_at"`
	// SecretExpiresAt is the RFC 7591 client_secret_expires_at: a Unix
	// second after which the secret is invalid, or zero when the secret
	// does not expire.
	SecretExpiresAt int64 `json:"client_secret_expires_at,omitempty"`
}

// secretExpired reports whether the record's client secret has passed its
// RFC 7591 expiry. An expired secret authenticates no exchange, so the
// record must not count as a usable registration.
func (c *Client) secretExpired(rec *ClientRecord) bool {
	return rec.SecretExpiresAt > 0 && c.clock().Unix() >= rec.SecretExpiresAt
}

// RegisteredClient returns the stored client registration, or found=false.
// A legacy record that predates the per-method secret check and lacks the
// secret its auth method requires is treated as absent: refresh and
// readiness fail as no-credential, and the next login re-registers, so an
// upgraded profile self-heals instead of looping on an unusable record.
func (c *Client) RegisteredClient() (*ClientRecord, bool, error) {
	data, found, err := c.secrets.GetSecret(labelClient)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	if !found {
		return nil, false, nil
	}
	var rec ClientRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, false, fmt.Errorf("stored client record is malformed: json")
	}
	if rec.ClientID == "" || rec.Issuer == "" || rec.AuthMethod == "" {
		return nil, false, fmt.Errorf("stored client record is incomplete")
	}
	if recordMissingSecret(&rec) || c.secretExpired(&rec) {
		return nil, false, nil
	}
	return &rec, true, nil
}

// recordMissingSecret reports whether the record's auth method requires a
// client secret the record does not carry.
func recordMissingSecret(rec *ClientRecord) bool {
	return (rec.AuthMethod == "client_secret_basic" || rec.AuthMethod == "client_secret_post") &&
		rec.ClientSecret == ""
}

// Register returns the stored registration when it still binds the same
// issuer, otherwise performs dynamic client registration and persists the
// result.
func (c *Client) Register(ctx context.Context, md *Metadata) (*ClientRecord, error) {
	if rec, found, err := c.RegisteredClient(); err != nil {
		return nil, err
	} else if found {
		if rec.Issuer != md.AS.Issuer {
			return nil, fmt.Errorf("%w: stored client registration binds a different issuer", ErrMetadata)
		}
		return rec, nil
	}

	body := map[string]any{
		"client_name":                "Tama Link",
		"redirect_uris":              []string{c.redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": md.AS.TokenEndpointAuthMethod(),
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode registration: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.AS.RegistrationEndpoint, strings.NewReader(string(data)))
	if err != nil {
		return nil, fmt.Errorf("build registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("register client: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := readBody(resp.Body, c.maxBytes)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registration endpoint answered %d", resp.StatusCode)
	}
	var created struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		// Zero means the secret does not expire (RFC 7591).
		SecretExpiresAt int64 `json:"client_secret_expires_at"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		return nil, fmt.Errorf("decode registration: json")
	}
	if created.ClientID == "" {
		return nil, fmt.Errorf("registration returned no client id")
	}
	rec := &ClientRecord{
		ClientID:        created.ClientID,
		ClientSecret:    created.ClientSecret,
		AuthMethod:      md.AS.TokenEndpointAuthMethod(),
		Issuer:          md.AS.Issuer,
		RegisteredAt:    c.clock().UTC(),
		SecretExpiresAt: created.SecretExpiresAt,
	}
	// A client-secret auth method without a secret would be persisted as a
	// permanently unusable registration: readiness would accept submits
	// that burn idempotency keys, and every token exchange would fail
	// locally on the missing secret. The same check re-validates stored
	// records on load, so a legacy record self-heals on the next login.
	if recordMissingSecret(rec) {
		return nil, fmt.Errorf("registration returned no client secret for %s", rec.AuthMethod)
	}
	// A replacement registration issues a new client: any refresh
	// credential still stored belongs to the previous client ID and can
	// never refresh under the new one. It is retired before the new record
	// is stored, so readiness never pairs the new registration with the
	// orphaned credential. The normal first-login path stores no
	// credential yet and is a no-op.
	if err := c.retireOrphanedCredential(ctx); err != nil {
		return nil, err
	}
	if err := c.storeClient(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// storeClient persists the registration record.
func (c *Client) storeClient(rec *ClientRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode client record: %w", err)
	}
	if err := c.secrets.SetSecret(labelClient, data); err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	return nil
}

// TokenEndpointAuthMethod returns the auth method this client will use:
// the selection discovery validated, shared with registration.
func (as *AuthorizationServer) TokenEndpointAuthMethod() string {
	return tokenEndpointAuthMethod(as.TokenEndpointAuthMethods)
}

// applyFormAuth adds the form-based client credentials for the auth method
// (RFC 7591 public clients send client_id; client_secret_post sends both).
// It must run before the form is encoded into the request body; the client
// secret never appears outside this function and applyHeaderAuth.
func (rec *ClientRecord) applyFormAuth(form url.Values) error {
	switch rec.AuthMethod {
	case "none":
		form.Set("client_id", rec.ClientID)
	case "client_secret_post":
		if rec.ClientSecret == "" {
			return fmt.Errorf("client secret missing for client_secret_post")
		}
		form.Set("client_id", rec.ClientID)
		form.Set("client_secret", rec.ClientSecret)
	case "client_secret_basic":
		// The credentials ride in the Authorization header instead.
	default:
		return fmt.Errorf("unsupported token endpoint auth method %q", rec.AuthMethod)
	}
	return nil
}

// applyHeaderAuth applies the header-based credentials for the auth method
// after the request is constructed.
func (rec *ClientRecord) applyHeaderAuth(req *http.Request) error {
	if rec.AuthMethod != "client_secret_basic" {
		return nil
	}
	if rec.ClientSecret == "" {
		return fmt.Errorf("client secret missing for client_secret_basic")
	}
	req.SetBasicAuth(rec.ClientID, rec.ClientSecret)
	return nil
}

// tokenResponse is the RFC 6749 token endpoint response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// postToken performs one token endpoint exchange and decodes the response.
// The provider's error description is deliberately not carried into the
// returned error: it is provider text that could embed request data.
func (c *Client) postToken(ctx context.Context, endpoint string, rec *ClientRecord, form url.Values) (*tokenResponse, error) {
	// Form-based credentials (none, client_secret_post) must be added before
	// the body is encoded; header-based credentials (client_secret_basic)
	// are applied after the request exists.
	if err := rec.applyFormAuth(form); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if err := rec.applyHeaderAuth(req); err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := readBody(resp.Body, c.maxBytes)
	if err != nil {
		return nil, err
	}
	var out tokenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode token response: json")
	}
	if out.AccessToken != "" {
		// The upstream transport unconditionally sends the access token as
		// a Bearer credential: an omitted or different token type would
		// pass readiness while every authenticated call fails.
		if !strings.EqualFold(out.TokenType, "bearer") {
			return nil, fmt.Errorf("token endpoint returned unsupported token type %q", out.TokenType)
		}
		return &out, nil
	}
	if out.Error == "" {
		return nil, fmt.Errorf("token endpoint answered %d with no token", resp.StatusCode)
	}
	if out.Error == "invalid_grant" {
		return nil, fmt.Errorf("%w", ErrGrantInvalid)
	}
	return nil, fmt.Errorf("token endpoint rejected the request: %s", out.Error)
}
