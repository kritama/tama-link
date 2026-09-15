package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ProtectedResource is the RFC 9728 protected-resource metadata document.
type ProtectedResource struct {
	Issuer               string   `json:"issuer"`
	AuthorizationServers []string `json:"authorization_servers"`
	Resource             string   `json:"resource"`
}

// AuthorizationServer is the RFC 8414 authorization-server metadata document.
type AuthorizationServer struct {
	Issuer                   string   `json:"issuer"`
	AuthorizationEndpoint    string   `json:"authorization_endpoint"`
	TokenEndpoint            string   `json:"token_endpoint"`
	RegistrationEndpoint     string   `json:"registration_endpoint"`
	CodeChallengeMethods     []string `json:"code_challenge_methods_supported"`
	GrantTypes               []string `json:"grant_types_supported"`
	ResponseTypeBindings     []string `json:"response_types_supported"`
	TokenEndpointAuthMethods []string `json:"token_endpoint_auth_methods_supported"`
}

// Metadata is the validated discovery result for one profile endpoint.
type Metadata struct {
	PRM ProtectedResource
	AS  AuthorizationServer
	// ASURL is the authorization server that was selected.
	ASURL string
}

// Discover fetches and validates protected-resource and authorization-server
// metadata for the profile endpoint. Validation is exact: the PRM resource
// must equal the endpoint, and the selected authorization server's issuer
// must equal the profile's expected issuer.
func (c *Client) Discover(ctx context.Context) (*Metadata, error) {
	prmURL, err := prmURL(c.endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	prmData, err := c.fetchMetadata(ctx, prmURL)
	if err != nil {
		return nil, err
	}
	var prm ProtectedResource
	if err := json.Unmarshal(prmData, &prm); err != nil {
		return nil, fmt.Errorf("%w: decode metadata: json", ErrMetadata)
	}
	if err := validatePRM(&prm, c.endpoint); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}

	asURL, err := c.selectAuthorizationServer(&prm)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	metadataURL, err := asMetadataURL(asURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	asData, err := c.fetchMetadata(ctx, metadataURL)
	if err != nil {
		return nil, err
	}
	var as AuthorizationServer
	if err := json.Unmarshal(asData, &as); err != nil {
		return nil, fmt.Errorf("%w: decode metadata: json", ErrMetadata)
	}
	if err := validateAS(&as, c.issuer); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	return &Metadata{PRM: prm, AS: as, ASURL: asURL}, nil
}

// validatePRM enforces the RFC 9728 invariants this client depends on.
func validatePRM(prm *ProtectedResource, endpoint string) error {
	if len(prm.AuthorizationServers) == 0 {
		return fmt.Errorf("metadata lists no authorization servers")
	}
	if prm.Resource != endpoint {
		return fmt.Errorf("protected-resource metadata does not bind this endpoint")
	}
	if prm.Issuer != "" && prm.Issuer != endpoint && prm.Issuer != endpointOrigin(endpoint) {
		return fmt.Errorf("protected-resource issuer does not match the endpoint")
	}
	return nil
}

// endpointOrigin is the scheme+host of the endpoint, the other issuer form
// a protected-resource document may use.
func endpointOrigin(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// selectAuthorizationServer picks the one server matching the expected
// issuer exactly.
func (c *Client) selectAuthorizationServer(prm *ProtectedResource) (string, error) {
	for _, candidate := range prm.AuthorizationServers {
		u, err := url.Parse(candidate)
		if err != nil {
			continue
		}
		if err := checkSecureURL(u); err != nil {
			continue
		}
		if candidate == c.issuer || strings.TrimSuffix(candidate, "/") == c.issuer {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no authorization server matches the expected issuer")
}

// validateAS enforces the RFC 8414 invariants this client depends on.
func validateAS(as *AuthorizationServer, issuer string) error {
	if as.Issuer != issuer {
		return fmt.Errorf("authorization-server issuer does not match the expected issuer")
	}
	if err := checkEndpoint(as.AuthorizationEndpoint); err != nil {
		return fmt.Errorf("authorization endpoint: %w", err)
	}
	if err := checkEndpoint(as.TokenEndpoint); err != nil {
		return fmt.Errorf("token endpoint: %w", err)
	}
	if as.RegistrationEndpoint == "" {
		return fmt.Errorf("authorization server provides no registration endpoint")
	}
	if err := checkEndpoint(as.RegistrationEndpoint); err != nil {
		return fmt.Errorf("registration endpoint: %w", err)
	}
	if !contains(as.CodeChallengeMethods, "S256") {
		return fmt.Errorf("authorization server does not support PKCE S256")
	}
	if !contains(as.GrantTypes, "authorization_code") || !contains(as.GrantTypes, "refresh_token") {
		return fmt.Errorf("authorization server does not support the required grants")
	}
	method := defaultAuthMethod(as.TokenEndpointAuthMethods)
	if !supportedAuthMethods[method] {
		return fmt.Errorf("unsupported token endpoint auth method %q", method)
	}
	return nil
}

// supportedAuthMethods are the token endpoint auth methods this client can
// use. Private-key JWT is deliberately excluded from the initial release.
var supportedAuthMethods = map[string]bool{
	"client_secret_basic": true,
	"client_secret_post":  true,
	"none":                true,
}

// defaultAuthMethod applies the RFC 8414 default when the document omits the
// field.
func defaultAuthMethod(methods []string) string {
	if len(methods) == 0 {
		return "client_secret_basic"
	}
	return methods[0]
}

// fetchMetadata GETs one metadata URL and returns the bounded JSON body.
func (c *Client) fetchMetadata(ctx context.Context, rawURL string) ([]byte, error) {
	if err := checkEndpoint(rawURL); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build metadata request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch metadata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w: metadata endpoint answered %d", ErrMetadata, resp.StatusCode)
	}
	data, err := readBody(resp.Body, c.maxBytes)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// readBody reads a response body up to bound and fails closed past it.
func readBody(body io.Reader, bound int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, bound+1))
	if err != nil {
		return nil, fmt.Errorf("read metadata body: %w", err)
	}
	if int64(len(data)) > bound {
		return nil, fmt.Errorf("response body exceeds %d bytes", bound)
	}
	return data, nil
}

// contains reports whether list contains value.
func contains(list []string, value string) bool {
	for _, v := range list {
		if v == value {
			return true
		}
	}
	return false
}

// checkSecureURL accepts https everywhere and http only for loopback
// origins, so local Tama instances work while real deployments stay
// encrypted.
func checkSecureURL(u *url.URL) error {
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("url must be http(s)")
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return fmt.Errorf("http is allowed only for loopback origins")
	}
	return nil
}

// checkEndpoint validates a string endpoint URL.
func checkEndpoint(raw string) error {
	if raw == "" {
		return fmt.Errorf("empty url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Host == "" {
		return fmt.Errorf("url has no host")
	}
	return checkSecureURL(u)
}

// isLoopback reports whether host is a loopback name or address.
func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}
