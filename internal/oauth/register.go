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
}

// RegisteredClient returns the stored client registration, or found=false.
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
	return &rec, true, nil
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
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		return nil, fmt.Errorf("decode registration: json")
	}
	if created.ClientID == "" {
		return nil, fmt.Errorf("registration returned no client id")
	}
	rec := &ClientRecord{
		ClientID:     created.ClientID,
		ClientSecret: created.ClientSecret,
		AuthMethod:   md.AS.TokenEndpointAuthMethod(),
		Issuer:       md.AS.Issuer,
		RegisteredAt: c.clock().UTC(),
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

// TokenEndpointAuthMethod returns the auth method this client will use.
func (as *AuthorizationServer) TokenEndpointAuthMethod() string {
	return defaultAuthMethod(as.TokenEndpointAuthMethods)
}

// applyAuth applies the token endpoint auth method to the request. The
// client secret never appears outside this function.
func (rec *ClientRecord) applyAuth(req *http.Request, form url.Values) error {
	switch rec.AuthMethod {
	case "client_secret_basic":
		req.SetBasicAuth(rec.ClientID, rec.ClientSecret)
	case "client_secret_post":
		if rec.ClientSecret == "" {
			return fmt.Errorf("client secret missing for client_secret_post")
		}
		form.Set("client_id", rec.ClientID)
		form.Set("client_secret", rec.ClientSecret)
	case "none":
		form.Set("client_id", rec.ClientID)
	default:
		return fmt.Errorf("unsupported token endpoint auth method %q", rec.AuthMethod)
	}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if err := rec.applyAuth(req, form); err != nil {
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
