package profile

import (
	"fmt"
	"sort"
)

// Bounds for the OAuth scopes a profile version 2 declares. The profile
// owner selects the least-privileged set; Tama Link never infers or silently
// requests every scope a server advertises.
const (
	// MaxScopes bounds how many scopes one profile may declare.
	MaxScopes = 16

	// MaxScopeLength bounds one scope token in bytes.
	MaxScopeLength = 255
)

// validScopeToken reports whether value is a bounded OAuth scope token:
// 1-255 bytes of ASCII alphanumerics plus dot, underscore, colon, slash, or
// hyphen. Whitespace and control characters delimit scope strings, so a
// token carrying them would corrupt the canonical wire value.
func validScopeToken(token string) bool {
	if token == "" || len(token) > MaxScopeLength {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == ':' || c == '/' || c == '-':
		default:
			return false
		}
	}
	return true
}

// CanonicalScopes validates a declared scope set and returns it sorted,
// the canonical order every profile digest and wire request uses.
// Duplicates are rejected rather than folded away silently.
func CanonicalScopes(scopes []string) ([]string, error) {
	if len(scopes) > MaxScopes {
		return nil, fmt.Errorf("scopes declare %d values, the limit is %d", len(scopes), MaxScopes)
	}
	canonical := make([]string, len(scopes))
	for i, token := range scopes {
		if !validScopeToken(token) {
			return nil, fmt.Errorf("scope %q is not a valid OAuth scope token", token)
		}
		canonical[i] = token
	}
	seen := make(map[string]bool, len(canonical))
	for _, token := range canonical {
		if seen[token] {
			return nil, fmt.Errorf("duplicate scope %q", token)
		}
		seen[token] = true
	}
	sort.Strings(canonical)
	return canonical, nil
}

// validateScopes enforces the version-specific scope contract and returns
// the canonical scope set the digest and wire requests use.
func validateScopes(version int, declared []string) ([]string, error) {
	if version == 2 {
		if len(declared) == 0 {
			return nil, fmt.Errorf(
				"version 2 profiles require a non-empty scopes array; regenerate the profile with its least-privileged OAuth scopes")
		}
		canonical, err := CanonicalScopes(declared)
		if err != nil {
			return nil, err
		}
		return canonical, nil
	}
	if len(declared) != 0 {
		return nil, fmt.Errorf(
			"version 1 profiles must not declare scopes; regenerate the profile as version 2 with an explicit scopes array")
	}
	return nil, nil
}
