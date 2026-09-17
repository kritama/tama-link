package oauth

import (
	"fmt"
	"net/url"
	"strings"
)

// prmURL builds the RFC 9728 protected-resource metadata URL for the
// endpoint: the well-known path followed by the endpoint path.
func prmURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse endpoint: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("endpoint must be absolute")
	}
	return u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource" + u.Path, nil
}

// asMetadataURL builds the RFC 8414 authorization-server metadata URL for an
// authorization server location, applying the path-preserving rules and
// stripping any trailing slash.
func asMetadataURL(asURL string) (string, error) {
	u, err := url.Parse(asURL)
	if err != nil {
		return "", fmt.Errorf("parse authorization server: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("authorization server must be absolute")
	}
	path := strings.TrimSuffix(u.Path, "/")
	return u.Scheme + "://" + u.Host + "/.well-known/oauth-authorization-server" + path, nil
}
