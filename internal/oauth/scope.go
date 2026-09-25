package oauth

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// maxAdvertisedScopes bounds one metadata document's scopes_supported list.
	maxAdvertisedScopes = 256

	// maxReturnedScopeToken bounds one token in a token endpoint's scope
	// declaration, matching the profile scope-token bound.
	maxReturnedScopeToken = 255
)

// validateAdvertisedScopes bounds and sanity-checks one metadata document's
// scopes_supported list. A missing field is not authoritative and passes; a
// present field, including an empty list, must be well formed.
func validateAdvertisedScopes(field string, list *[]string) error {
	if list == nil {
		return nil
	}
	if len(*list) > maxAdvertisedScopes {
		return fmt.Errorf("%s advertises %d scopes, the limit is %d", field, len(*list), maxAdvertisedScopes)
	}
	for _, token := range *list {
		if !validAdvertisedScopeToken(token) {
			return fmt.Errorf("%s advertises the malformed scope %q", field, token)
		}
	}
	return nil
}

// validAdvertisedScopeToken accepts one bounded printable-ASCII token. The
// full profile token grammar is not required of a server's advertisement;
// only size and control/whitespace hygiene are, because subset checks
// compare exact strings.
func validAdvertisedScopeToken(token string) bool {
	if token == "" || len(token) > maxReturnedScopeToken {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if c <= ' ' || c > 127 {
			return false
		}
	}
	return true
}

// CheckRequestedScopes requires the profile's requested scopes to be a
// subset of every advertised scope list that is present, in both the
// protected-resource and authorization-server documents. A missing field is
// not authoritative and is skipped; a present list, including an empty one,
// must never be ignored. When the profile requests no scopes (a version 1
// profile), there is nothing to bind and the check passes.
func (m *Metadata) CheckRequestedScopes(requested []string) error {
	if len(requested) == 0 {
		return nil
	}
	for _, list := range []*[]string{m.PRM.ScopesSupported, m.AS.ScopesSupported} {
		if list == nil {
			continue
		}
		for _, scope := range requested {
			if !contains(*list, scope) {
				return fmt.Errorf("the requested scope %q is not supported by the authorization server", scope)
			}
		}
	}
	return nil
}

// canonicalReturnedScopes decodes a token endpoint's scope declaration:
// RFC 6749 single-space-separated tokens with no empty entries. The result
// is deduplicated and sorted so set comparison never depends on order.
func canonicalReturnedScopes(declared string) ([]string, error) {
	tokens := strings.Split(declared, " ")
	out := make([]string, 0, len(tokens))
	seen := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		if token == "" {
			return nil, fmt.Errorf("the scope declaration has an empty token")
		}
		if len(token) > maxReturnedScopeToken {
			return nil, fmt.Errorf("the scope declaration carries an oversized token")
		}
		if seen[token] {
			continue
		}
		seen[token] = true
		out = append(out, token)
	}
	sort.Strings(out)
	return out, nil
}

// credentialScopesBound reports whether a stored refresh credential's bound
// scope set is the active client's: a client without a requested set
// (a version 1 profile) retains its legacy behavior, while a scoped client
// requires the exact canonical set, so a grant with broader or stale
// privileges never survives profile reconciliation.
func credentialScopesBound(bound, active []string) bool {
	if len(active) == 0 {
		return true
	}
	return scopeSetEqual(active, bound)
}

// scopeSetEqual reports whether two canonical scope sets are identical.
func scopeSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// checkReturnedScope validates a token response's scope declaration against
// the profile's requested set and returns the granted set to bind to the
// refresh credential. Only an omitted declaration inherits the requested
// set. A present declaration — including an explicit null or empty string —
// must be well formed and equal the requested set exactly: a reduced,
// expanded, or malformed set fails closed, so privilege changes can never
// occur silently. When the profile requests no scopes (a version 1
// profile), the declaration is not a binding and is ignored.
func (c *Client) checkReturnedScope(declared declaredScope) ([]string, error) {
	if len(c.scopes) == 0 {
		return nil, nil
	}
	if !declared.present {
		return c.scopes, nil
	}
	got, err := canonicalReturnedScopes(declared.value)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrScopeMismatch, err)
	}
	if !scopeSetEqual(got, c.scopes) {
		return nil, fmt.Errorf("%w: the token endpoint returned a different scope set than the profile requested", ErrScopeMismatch)
	}
	return got, nil
}

// checkBoundScope validates a refresh response's scope declaration against
// the set already bound to the durable refresh credential. An omitted
// declaration keeps the bound set. A present declaration must be well
// formed and equal the bound set exactly. A credential without a bound set
// predates scope binding, so its declaration is not a binding and passes.
func checkBoundScope(bound []string, declared declaredScope) error {
	if len(bound) == 0 {
		return nil
	}
	if !declared.present {
		return nil
	}
	got, err := canonicalReturnedScopes(declared.value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrScopeMismatch, err)
	}
	if !scopeSetEqual(got, bound) {
		return fmt.Errorf("%w: the token endpoint returned a different scope set than the credential bound", ErrScopeMismatch)
	}
	return nil
}

// registrationScopeOK validates a dynamic-registration response's scope
// declaration: when present it must be well formed and support every scope
// the profile requested, so a registration that cannot grant the request is
// rejected before the browser opens. An omitted declaration passes.
func registrationScopeOK(requested []string, declared declaredScope) error {
	if len(requested) == 0 || !declared.present {
		return nil
	}
	got, err := canonicalReturnedScopes(declared.value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrScopeMismatch, err)
	}
	for _, scope := range requested {
		if !contains(got, scope) {
			return fmt.Errorf("%w: the registration does not support the requested scope %q", ErrScopeMismatch, scope)
		}
	}
	return nil
}
