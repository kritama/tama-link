package profile

import (
	"fmt"
	"net/url"
	"strings"
)

// ParseOrigin validates a path-free Tama instance address and returns the
// normalized https origin. A trailing slash is accepted and removed. Any
// other path, including an MCP endpoint path, is rejected so the caller
// passes the origin and profile type separately.
func ParseOrigin(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("address %q is not a URL: %v", raw, err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("address %q must use https", raw)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("address %q must not contain credentials", raw)
	}
	if parsed.Hostname() == "" {
		return "", fmt.Errorf("address %q has no host", raw)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("address %q must not contain a query or fragment", raw)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("address %q must be a path-free origin; pass the origin and --type separately", raw)
	}
	origin := parsed.Scheme + "://" + parsed.Host
	if _, err := checkHTTPS("origin", origin, false); err != nil {
		return "", err
	}
	return origin, nil
}

// EndpointFor derives the exact MCP endpoint for kind from a validated
// origin. app selects /mcp/app and system selects /mcp/system.
func EndpointFor(origin string, kind Kind) (string, error) {
	normalized, err := ParseOrigin(origin)
	if err != nil {
		return "", err
	}
	switch kind {
	case KindApp:
		return normalized + "/mcp/app", nil
	case KindSystem:
		return normalized + "/mcp/system", nil
	default:
		return "", fmt.Errorf("unsupported profile type %q", kind)
	}
}

// ParseSecureURL parses value and requires https without user info, query,
// or fragment. allowPath permits a non-empty path for endpoints and issuers.
func ParseSecureURL(field, value string, allowPath bool) (*url.URL, error) {
	return checkHTTPS(field, value, allowPath)
}

// ParseKind accepts the bootstrap type flag. The only values are app and
// system; there is no arbitrary endpoint path.
func ParseKind(value string) (Kind, error) {
	switch Kind(strings.TrimSpace(value)) {
	case KindApp:
		return KindApp, nil
	case KindSystem:
		return KindSystem, nil
	default:
		return "", fmt.Errorf("unsupported profile type %q: use app or system", value)
	}
}
