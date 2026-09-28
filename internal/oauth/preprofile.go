package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/kritama/tama-link/internal/profile"
)

// DiscoverProtectedResource fetches and validates RFC 9728 metadata for the
// exact endpoint before a profile or expected issuer exists. Redirects are
// refused. The metadata resource must equal endpoint.
func DiscoverProtectedResource(ctx context.Context, endpoint string, client *http.Client) (*ProtectedResource, error) {
	if _, err := profile.ParseSecureURL("endpoint", endpoint, true); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	prmURL, err := prmURL(endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	data, err := fetchDiscoveredMetadata(ctx, client, prmURL)
	if err != nil {
		return nil, err
	}
	var prm ProtectedResource
	if err := json.Unmarshal(data, &prm); err != nil {
		return nil, fmt.Errorf("%w: decode metadata: json", ErrMetadata)
	}
	if err := validatePRM(&prm, endpoint); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	if _, err := ValidAuthorizationServers(&prm); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	return &prm, nil
}

// ValidAuthorizationServers returns every advertised authorization-server
// URL after validating each one. An invalid candidate rejects the document
// rather than being skipped or displayed.
func ValidAuthorizationServers(prm *ProtectedResource) ([]string, error) {
	if prm == nil || len(prm.AuthorizationServers) == 0 {
		return nil, fmt.Errorf("metadata lists no authorization servers")
	}
	out := make([]string, 0, len(prm.AuthorizationServers))
	seen := make(map[string]bool, len(prm.AuthorizationServers))
	for _, candidate := range prm.AuthorizationServers {
		if _, err := profile.ParseSecureURL("authorization server", candidate, true); err != nil {
			return nil, err
		}
		if seen[candidate] {
			return nil, fmt.Errorf("metadata repeats an authorization server")
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	return out, nil
}

// DiscoverAuthorizationServer fetches and validates RFC 8414 metadata for
// one advertised authorization-server URL. The document issuer is pinned
// only when it equals that URL. The token endpoint remains same-origin
// with the issuer.
func DiscoverAuthorizationServer(ctx context.Context, asURL string, client *http.Client) (*AuthorizationServer, error) {
	if _, err := profile.ParseSecureURL("authorization server", asURL, true); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	metadataURL, err := asMetadataURL(asURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	data, err := fetchDiscoveredMetadata(ctx, client, metadataURL)
	if err != nil {
		return nil, err
	}
	var as AuthorizationServer
	if err := json.Unmarshal(data, &as); err != nil {
		return nil, fmt.Errorf("%w: decode metadata: json", ErrMetadata)
	}
	if as.Issuer == "" || !advertisedIssuerSelected(asURL, as.Issuer) {
		return nil, fmt.Errorf("%w: authorization-server issuer does not match the selected server", ErrMetadata)
	}
	if _, err := profile.ParseSecureURL("issuer", as.Issuer, true); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	if err := validateAS(&as, as.Issuer); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	return &as, nil
}

func fetchDiscoveredMetadata(ctx context.Context, base *http.Client, rawURL string) ([]byte, error) {
	if err := checkEndpoint(rawURL); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMetadata, err)
	}
	client := refusingClient(base)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build metadata request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch metadata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("%w: refusing redirect", ErrMetadata)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: metadata endpoint answered %d", ErrMetadata, resp.StatusCode)
	}
	data, err := readBody(resp.Body, DefaultMaxMetadataBytes)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func refusingClient(base *http.Client) *http.Client {
	if base == nil {
		base = &http.Client{Timeout: 30 * time.Second}
	}
	cloned := *base
	cloned.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	if cloned.Timeout == 0 {
		cloned.Timeout = 30 * time.Second
	}
	return &cloned
}
