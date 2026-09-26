package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssuerSelectionMatchesRuntimeDiscovery(t *testing.T) {
	t.Parallel()
	accepted := []struct {
		advertised string
		issuer     string
	}{
		{"https://issuer.example", "https://issuer.example"},
		{"https://issuer.example/", "https://issuer.example"},
	}
	for _, tc := range accepted {
		if !advertisedIssuerSelected(tc.advertised, tc.issuer) {
			t.Fatalf("pre-profile rejected selectable issuer advertised %s pinned %s", tc.advertised, tc.issuer)
		}
		client := &Client{issuer: tc.issuer}
		got, err := client.selectAuthorizationServer(&ProtectedResource{AuthorizationServers: []string{tc.advertised}})
		if err != nil || got != tc.advertised {
			t.Fatalf("runtime selection of %s for %s = %q, %v", tc.advertised, tc.issuer, got, err)
		}
	}
	if advertisedIssuerSelected("https://issuer.example", "https://issuer.example/") {
		t.Fatal("accepted a pinned issuer that runtime discovery cannot select")
	}
	client := &Client{issuer: "https://issuer.example/"}
	if _, err := client.selectAuthorizationServer(&ProtectedResource{AuthorizationServers: []string{"https://issuer.example"}}); err == nil {
		t.Fatal("runtime selected an issuer with only a trailing slash")
	}
}

func TestDiscoverAuthorizationServerRejectsSlashOnlyOnDocumentIssuer(t *testing.T) {
	t.Parallel()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		issuer := srv.URL + "/"
		_, _ = w.Write([]byte(`{
			"issuer":"` + issuer + `",
			"authorization_endpoint":"` + srv.URL + `/authorize",
			"token_endpoint":"` + srv.URL + `/token",
			"registration_endpoint":"` + srv.URL + `/register",
			"code_challenge_methods_supported":["S256"],
			"grant_types_supported":["authorization_code","refresh_token"],
			"token_endpoint_auth_methods_supported":["none"]
		}`))
	}))
	t.Cleanup(srv.Close)
	_, err := DiscoverAuthorizationServer(context.Background(), srv.URL, srv.Client())
	if err == nil {
		t.Fatal("accepted a document issuer runtime discovery cannot select")
	}
}

func TestDiscoverProtectedResourceRejectsRedirect(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/metadata", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	endpoint := srv.URL + "/mcp/app"
	_, err := DiscoverProtectedResource(context.Background(), endpoint, srv.Client())
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("error = %v, want redirect rejection", err)
	}
}

func TestDiscoverProtectedResourcePinsIssuer(t *testing.T) {
	t.Parallel()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp/app":
			_, _ = w.Write([]byte(`{"resource":"` + srv.URL + `/mcp/app","authorization_servers":["` + srv.URL + `"],"scopes_supported":["mcp.message"]}`))
		case "/.well-known/oauth-authorization-server":
			_, _ = w.Write([]byte(`{
				"issuer":"` + srv.URL + `",
				"authorization_endpoint":"` + srv.URL + `/authorize",
				"token_endpoint":"` + srv.URL + `/token",
				"registration_endpoint":"` + srv.URL + `/register",
				"code_challenge_methods_supported":["S256"],
				"grant_types_supported":["authorization_code","refresh_token"],
				"token_endpoint_auth_methods_supported":["none"],
				"scopes_supported":["mcp.message"]
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	endpoint := srv.URL + "/mcp/app"
	prm, err := DiscoverProtectedResource(context.Background(), endpoint, srv.Client())
	if err != nil {
		t.Fatalf("DiscoverProtectedResource: %v", err)
	}
	candidates, err := ValidAuthorizationServers(prm)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %v, err %v", candidates, err)
	}
	as, err := DiscoverAuthorizationServer(context.Background(), candidates[0], srv.Client())
	if err != nil {
		t.Fatalf("DiscoverAuthorizationServer: %v", err)
	}
	if as.Issuer != srv.URL {
		t.Fatalf("issuer = %s", as.Issuer)
	}
}
