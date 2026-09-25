package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestNewClientCanonicalizesScopes(t *testing.T) {
	t.Parallel()

	c, err := New(Config{
		Endpoint: testEndpoint,
		Issuer:   testIssuer,
		Secrets:  newFakeSecrets(),
		Lease:    newFakeLease(),
		Scopes:   []string{"zeta.scope", "alpha.scope"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if want := []string{"alpha.scope", "zeta.scope"}; !scopeSetEqual(c.scopes, want) {
		t.Fatalf("scopes = %v, want %v", c.scopes, want)
	}
	if c.scope != "alpha.scope zeta.scope" {
		t.Fatalf("scope string = %q", c.scope)
	}
}

func TestNewClientRejectsMalformedScopes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		scopes []string
	}{
		{"duplicates", []string{"a", "b", "a"}},
		{"whitespace token", []string{"mcp message"}},
		{"control token", []string{"a\x7fb"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(Config{
				Endpoint: testEndpoint,
				Issuer:   testIssuer,
				Secrets:  newFakeSecrets(),
				Lease:    newFakeLease(),
				Scopes:   tc.scopes,
			}); err == nil {
				t.Fatalf("New accepted %v", tc.scopes)
			}
		})
	}
}

func TestAuthorizationRequestCarriesCanonicalScope(t *testing.T) {
	t.Parallel()

	md := serverMetadata(testBase)
	redirect := "http://127.0.0.1:51234/callback"
	rec := &ClientRecord{ClientID: "cid-1", AuthMethod: "client_secret_basic", Issuer: testIssuer}

	c, err := New(Config{
		Endpoint: testEndpoint,
		Issuer:   testIssuer,
		Secrets:  newFakeSecrets(),
		Lease:    newFakeLease(),
		Scopes:   []string{"zeta.scope", "alpha.scope"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req, err := c.NewAuthorizationRequest(md, rec, redirect)
	if err != nil {
		t.Fatalf("NewAuthorizationRequest: %v", err)
	}
	if got := req.URL.Query().Get("scope"); got != "alpha.scope zeta.scope" {
		t.Fatalf("scope = %q, want the canonical space-joined set", got)
	}

	// A version 1 profile requests no scopes: the parameter is absent, not
	// empty, so the server applies its own defaults.
	legacy := newStaticClient(t, newFakeSecrets(), newFakeLease(), newTestClock(time.Now()))
	legacyReq, err := legacy.NewAuthorizationRequest(md, rec, redirect)
	if err != nil {
		t.Fatalf("NewAuthorizationRequest: %v", err)
	}
	if got := legacyReq.URL.Query().Get("scope"); got != "" {
		t.Fatalf("legacy scope = %q, want absent", got)
	}
}

// scopePRM and scopeAS build one metadata document per server, where
// scopesJSON is the scopes_supported field's JSON array or the empty
// string when the document omits the field.
func scopePRM(serverURL, scopesJSON string) string {
	base := serverPRM(serverURL)
	if scopesJSON == "" {
		return base
	}
	return insertField(base, "scopes_supported", scopesJSON)
}

func scopeAS(serverURL, scopesJSON string) string {
	base := serverAS(serverURL)
	if scopesJSON == "" {
		return base
	}
	return insertField(base, "scopes_supported", scopesJSON)
}

// insertField appends one "name": value pair to a flat JSON object literal.
func insertField(jsonLiteral, name, value string) string {
	if i := stringsLastByte(jsonLiteral, '{'); i >= 0 {
		return jsonLiteral[:i+1] + `"` + name + `":` + value + "," + jsonLiteral[i+1:]
	}
	return jsonLiteral
}

func stringsLastByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// TestDiscoveryScopeAdvertisements proves the scopes_supported presence
// semantics through real discovery: absent is not authoritative, a present
// list including an empty one must be satisfied, and malformed
// advertisements fail validation.
func TestDiscoveryScopeAdvertisements(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		prmScopes string
		asScopes  string
		requested []string
		wantErr   bool
	}{
		{name: "absent in both documents", requested: []string{"mcp.message"}},
		{
			name:      "present in both and superset",
			prmScopes: `["mcp.message","other.scope"]`,
			asScopes:  `["mcp.message","other.scope"]`,
			requested: []string{"mcp.message"},
		},
		{
			name:      "present in both and exact",
			prmScopes: `["mcp.message"]`,
			asScopes:  `["mcp.message"]`,
			requested: []string{"mcp.message"},
		},
		{
			name:      "present in one document only",
			asScopes:  `["mcp.message"]`,
			requested: []string{"mcp.message"},
		},
		{
			name:      "advertised set missing a requested scope",
			prmScopes: `["other.scope"]`,
			asScopes:  `["mcp.message","other.scope"]`,
			requested: []string{"mcp.message"},
			wantErr:   true,
		},
		{
			name:      "present but empty is not ignored",
			prmScopes: `[]`,
			requested: []string{"mcp.message"},
			wantErr:   true,
		},
		{
			name:      "malformed advertised token",
			asScopes:  `["bad scope"]`,
			requested: []string{"mcp.message"},
			wantErr:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := (&metadataServer{}).start(t)
			server.prm = scopePRM(server.ts.URL, tc.prmScopes)
			server.as = scopeAS(server.ts.URL, tc.asScopes)

			client := clientForServerScopes(t, server, newFakeSecrets(), newFakeLease(), tc.requested, newTestClock(time.Now()))
			md, err := client.Discover(context.Background())
			if err == nil && tc.wantErr {
				// Discovery validates format only; the login boundary then
				// binds the requested set against the advertisements.
				err = md.CheckRequestedScopes(tc.requested)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("discovery plus binding accepted: prm=%s as=%s", tc.prmScopes, tc.asScopes)
				}
				return
			}
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if err := md.CheckRequestedScopes(tc.requested); err != nil {
				t.Fatalf("CheckRequestedScopes: %v", err)
			}
		})
	}
}

func clientForServerScopes(t *testing.T, server *metadataServer, secrets *fakeSecrets, lease *fakeLease, scopes []string, clock *testClock) *Client {
	t.Helper()
	client, err := New(Config{
		Endpoint:   server.ts.URL + "/mcp/app",
		Issuer:     server.ts.URL + "/oauth",
		Secrets:    secrets,
		Lease:      lease,
		Clock:      clock.nowFn(),
		HTTPClient: &http.Client{},
		Scopes:     scopes,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func TestCheckRequestedScopesTable(t *testing.T) {
	t.Parallel()

	list := []string{"a", "b"}
	md := serverMetadata(testBase)
	md.PRM.ScopesSupported = &list
	md.AS.ScopesSupported = nil

	if err := md.CheckRequestedScopes(nil); err != nil {
		t.Fatalf("no requested scopes: %v", err)
	}
	if err := md.CheckRequestedScopes([]string{"b"}); err != nil {
		t.Fatalf("subset: %v", err)
	}
	if err := md.CheckRequestedScopes([]string{"a", "b"}); err != nil {
		t.Fatalf("exact: %v", err)
	}
	if err := md.CheckRequestedScopes([]string{"c"}); err == nil {
		t.Fatal("unsupported scope accepted")
	}

	empty := []string{}
	md = serverMetadata(testBase)
	md.AS.ScopesSupported = &empty
	if err := md.CheckRequestedScopes([]string{"a"}); err == nil {
		t.Fatal("a present-empty advertisement must reject every requested scope")
	}
}

func TestIssuerResponseRequired(t *testing.T) {
	t.Parallel()

	md := serverMetadata(testBase)
	if md.IssuerResponseRequired() {
		t.Fatal("an absent advertisement must not require the issuer response parameter")
	}
	empty := []string{}
	md.AS.AuthorizationServerIssuersSupported = empty
	if md.IssuerResponseRequired() {
		t.Fatal("an empty advertisement must not require the issuer response parameter")
	}
	md.AS.AuthorizationServerIssuersSupported = []string{testBase + "/oauth", "https://other.example"}
	if !md.IssuerResponseRequired() {
		t.Fatal("a matching advertisement must require the issuer response parameter")
	}

	// A non-empty advertisement that does not include the validated issuer
	// is self-contradictory and fails discovery validation.
	server := (&metadataServer{}).start(t)
	server.prm = serverPRM(server.ts.URL)
	server.as = fmt.Sprintf(`{
		"issuer": %q,
		"authorization_endpoint": %q,
		"token_endpoint": %q,
		"registration_endpoint": %q,
		"code_challenge_methods_supported": ["S256"],
		"grant_types_supported": ["authorization_code", "refresh_token"],
		"response_types_supported": ["code"],
		"token_endpoint_auth_methods_supported": ["client_secret_basic"],
		"authorization_server_issuers_supported": ["https://other.example"]
	}`, server.ts.URL+"/oauth", server.ts.URL+"/oauth/authorize",
		server.ts.URL+"/oauth/token", server.ts.URL+"/oauth/register")
	client := clientForServer(t, server, newFakeSecrets(), newFakeLease(), newTestClock(time.Now()))
	if _, err := client.Discover(context.Background()); err == nil {
		t.Fatal("discovery accepted an issuer advertisement without the validated issuer")
	}
}

// TestCompleteAuthorizationReturnedScope proves the token endpoint's
// returned scope is bound to the profile's requested set: omission
// inherits it, an exact set in any order passes, and a reduced, expanded,
// or malformed set fails closed without committing the grant.
func TestCompleteAuthorizationReturnedScope(t *testing.T) {
	t.Parallel()

	run := func(name, tokenScope string, wantErr error) {
		t.Run(name, func(t *testing.T) {
			server := (&metadataServer{}).start(t)
			if tokenScope == "" {
				server.tokenBody = `{"access_token":"at-1","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-1"}`
			} else {
				server.tokenBody = fmt.Sprintf(
					`{"access_token":"at-1","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-1","scope":%q}`, tokenScope)
			}
			secrets := newFakeSecrets()
			lease := newFakeLease()
			client := clientForServerScopes(t, server, secrets, lease, []string{"mcp.message"}, newTestClock(time.Unix(1_700_000_000, 0)))
			rec := &ClientRecord{ClientID: "cid-1", ClientSecret: "shh", AuthMethod: "client_secret_basic", Issuer: client.issuer}
			storeTestClient(t, client, rec)
			md := serverMetadata(server.ts.URL)

			observed := "http://127.0.0.1:51234/callback"
			authReq, err := client.NewAuthorizationRequest(md, rec, observed)
			if err != nil {
				t.Fatalf("NewAuthorizationRequest: %v", err)
			}
			err = client.CompleteAuthorization(context.Background(), md, rec, authReq, "code-1", observed)
			if wantErr != nil {
				if !errors.Is(err, wantErr) {
					t.Fatalf("err = %v, want %v", err, wantErr)
				}
				if _, found, _ := secrets.GetSecret(labelRefresh); found {
					t.Fatal("a rejected scope set committed the grant")
				}
				return
			}
			if err != nil {
				t.Fatalf("CompleteAuthorization: %v", err)
			}
			var cred refreshCredential
			if err := json.Unmarshal([]byte(liveCredential(t, lease, secrets)), &cred); err != nil {
				t.Fatalf("decode credential: %v", err)
			}
			want := []string{"mcp.message"}
			if tokenScope != "" {
				want = canonicalMust(t, tokenScope)
			}
			if !scopeSetEqual(cred.Scopes, want) {
				t.Fatalf("bound scopes = %v, want %v", cred.Scopes, want)
			}
		})
	}
	run("omitted scope inherits the request", "", nil)
	run("exact scope in server order", "mcp.message", nil)
	run("reduced scope fails closed", "narrower.scope", ErrScopeMismatch)
	run("expanded scope fails closed", "mcp.message extra.scope", ErrScopeMismatch)
	run("different single scope fails closed", "other.scope", ErrScopeMismatch)
	run("malformed empty token fails closed", "mcp.message  extra", ErrScopeMismatch)
}

func TestCompleteAuthorizationLegacyClientIgnoresScope(t *testing.T) {
	t.Parallel()

	server := (&metadataServer{}).start(t)
	server.tokenBody = `{"access_token":"at-1","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-1","scope":"whatever.scope"}`
	secrets := newFakeSecrets()
	lease := newFakeLease()
	client := clientForServer(t, server, secrets, lease, newTestClock(time.Unix(1_700_000_000, 0)))
	rec := &ClientRecord{ClientID: "cid-1", ClientSecret: "shh", AuthMethod: "client_secret_basic", Issuer: client.issuer}
	storeTestClient(t, client, rec)
	md := serverMetadata(server.ts.URL)

	observed := "http://127.0.0.1:51234/callback"
	authReq, err := client.NewAuthorizationRequest(md, rec, observed)
	if err != nil {
		t.Fatalf("NewAuthorizationRequest: %v", err)
	}
	if err := client.CompleteAuthorization(context.Background(), md, rec, authReq, "code-1", observed); err != nil {
		t.Fatalf("CompleteAuthorization: %v", err)
	}
	var cred refreshCredential
	if err := json.Unmarshal([]byte(liveCredential(t, lease, secrets)), &cred); err != nil {
		t.Fatalf("decode credential: %v", err)
	}
	if cred.Scopes != nil {
		t.Fatalf("legacy credential bound scopes %v, want none", cred.Scopes)
	}
}

// TestRefreshScopeBinding proves refresh revalidates the token endpoint's
// returned scope against the set bound to the durable credential.
func TestRefreshScopeBinding(t *testing.T) {
	t.Parallel()

	run := func(name string, credScopes []string, tokenScope string, wantErr error) {
		t.Run(name, func(t *testing.T) {
			server := (&metadataServer{}).start(t)
			if tokenScope == "" {
				server.tokenBody = `{"access_token":"at-2","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2"}`
			} else {
				server.tokenBody = fmt.Sprintf(
					`{"access_token":"at-2","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2","scope":%q}`, tokenScope)
			}
			secrets := newFakeSecrets()
			client := clientForServer(t, server, secrets, newFakeLease(), newTestClock(time.Unix(1_700_000_000, 0)))
			cred := refreshCredential{
				RefreshToken:  "rt-1",
				TokenEndpoint: server.ts.URL + "/oauth/token",
				Issuer:        server.ts.URL + "/oauth",
				Scopes:        credScopes,
				Updated:       time.Unix(1_700_000_000, 0).UTC(),
			}
			credData, err := json.Marshal(cred)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := secrets.SetSecret(labelRefresh, credData); err != nil {
				t.Fatalf("store credential: %v", err)
			}
			rec := ClientRecord{ClientID: "cid-1", ClientSecret: "shh", AuthMethod: "client_secret_basic", Issuer: server.ts.URL + "/oauth"}
			recData, err := json.Marshal(rec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if err := secrets.SetSecret(labelClient, recData); err != nil {
				t.Fatalf("store record: %v", err)
			}

			_, err = client.Refresh(context.Background())
			if wantErr != nil {
				if !errors.Is(err, wantErr) {
					t.Fatalf("err = %v, want %v", err, wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}
		})
	}
	run("bound set exact", []string{"mcp.message"}, "mcp.message", nil)
	run("bound set omitted by server", []string{"mcp.message"}, "", nil)
	run("bound set reduced by server", []string{"mcp.message"}, "narrower.scope", ErrScopeMismatch)
	run("bound set expanded by server", []string{"mcp.message"}, "mcp.message extra.scope", ErrScopeMismatch)
	run("unbound legacy credential accepts anything", nil, "anything.scope", nil)
}

func TestRegisterScopeValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		declared string
		wantErr  bool
	}{
		{"omitted", "", false},
		{"supports the request", "mcp.message other.scope", false},
		{"exact", "mcp.message", false},
		{"missing a requested scope", "other.scope", true},
		{"malformed", "mcp.message  gap", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := registrationScopeOK([]string{"mcp.message"}, tc.declared)
			if tc.wantErr != (err != nil) {
				t.Fatalf("registrationScopeOK = %v, wantErr=%v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrScopeMismatch) {
				t.Fatalf("err = %v, want ErrScopeMismatch", err)
			}
		})
	}
}

func canonicalMust(t *testing.T, declared string) []string {
	t.Helper()
	out, err := canonicalReturnedScopes(declared)
	if err != nil {
		t.Fatalf("canonicalReturnedScopes(%q): %v", declared, err)
	}
	return out
}
