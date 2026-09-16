package oauth

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
	"time"
)

// parseTokenForm decodes the recorded raw token request body.
func parseTokenForm(t *testing.T, server *metadataServer) url.Values {
	t.Helper()
	server.tokenReqMu.Lock()
	raw := make([]byte, len(server.tokenRaw))
	copy(raw, server.tokenRaw)
	server.tokenReqMu.Unlock()
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		t.Fatalf("parse token form: %v", err)
	}
	return form
}

// assertExactForm compares the recorded token request body with the exact
// expected form, byte for byte.
func assertExactForm(t *testing.T, server *metadataServer, want url.Values) {
	t.Helper()
	if got := parseTokenForm(t, server).Encode(); got != want.Encode() {
		t.Errorf("token form = %s, want %s", got, want.Encode())
	}
}

// TestTokenExchangePublicClientNone covers the intended Tama dynamic-client
// flow: token_endpoint_auth_method none. Public clients send client_id in
// the request body for both the authorization-code and the refresh
// exchange, and no Authorization header.
func TestTokenExchangePublicClientNone(t *testing.T) {
	t.Run("authorization code", func(t *testing.T) {
		server := (&metadataServer{}).start(t)
		server.tokenBody = `{"access_token":"at-1","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-1"}`
		client := clientForServer(t, server, newFakeSecrets(), newFakeLease(), newTestClock(time.Unix(1_700_000_000, 0)))
		rec := &ClientRecord{ClientID: "cid-none", AuthMethod: "none", Issuer: client.issuer}
		md := serverMetadata(server.ts.URL)
		redirect := "http://127.0.0.1:51234/callback"
		req, err := client.NewAuthorizationRequest(md, rec, redirect)
		if err != nil {
			t.Fatalf("NewAuthorizationRequest: %v", err)
		}
		if err := client.CompleteAuthorization(context.Background(), md, rec, req, "code-1", redirect); err != nil {
			t.Fatalf("CompleteAuthorization: %v", err)
		}
		assertExactForm(t, server, url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {"code-1"},
			"redirect_uri":  {redirect},
			"code_verifier": {req.Verifier},
			"resource":      {client.endpoint},
			"client_id":     {"cid-none"},
		})
		if auth := tokenAuthHeader(t, server); auth != "" {
			t.Errorf("public client sent Authorization header %q", auth)
		}
	})

	t.Run("refresh", func(t *testing.T) {
		server := (&metadataServer{}).start(t)
		server.tokenBody = `{"access_token":"at-2","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2"}`
		secrets := newFakeSecrets()
		client := clientForServer(t, server, secrets, newFakeLease(), newTestClock(time.Unix(1_700_000_000, 0)))
		seedMethodCredentials(t, secrets, "cid-none", "", "none", client.issuer, server.ts.URL+"/oauth/token", "rt-1")

		if _, err := client.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		assertExactForm(t, server, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {"rt-1"},
			"resource":      {client.endpoint},
			"client_id":     {"cid-none"},
		})
		if auth := tokenAuthHeader(t, server); auth != "" {
			t.Errorf("public client sent Authorization header %q", auth)
		}
	})
}

// TestTokenExchangeClientSecretPost covers the second form-based method:
// client_id and client_secret both ride in the body, with no header.
func TestTokenExchangeClientSecretPost(t *testing.T) {
	server := (&metadataServer{}).start(t)
	server.tokenBody = `{"access_token":"at-1","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-1"}`
	secrets := newFakeSecrets()
	client := clientForServer(t, server, secrets, newFakeLease(), newTestClock(time.Unix(1_700_000_000, 0)))
	rec := &ClientRecord{ClientID: "cid-post", ClientSecret: "shh", AuthMethod: "client_secret_post", Issuer: client.issuer}
	seedMethodCredentials(t, secrets, "cid-post", "shh", "client_secret_post", client.issuer, server.ts.URL+"/oauth/token", "rt-0")

	// Refresh exchange asserts the exact form.
	if _, err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	assertExactForm(t, server, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"rt-0"},
		"resource":      {client.endpoint},
		"client_id":     {"cid-post"},
		"client_secret": {"shh"},
	})
	if auth := tokenAuthHeader(t, server); auth != "" {
		t.Errorf("client_secret_post sent Authorization header %q", auth)
	}

	// Authorization-code exchange carries the same credentials in the body.
	md := serverMetadata(server.ts.URL)
	redirect := "http://127.0.0.1:51234/callback"
	req, err := client.NewAuthorizationRequest(md, rec, redirect)
	if err != nil {
		t.Fatalf("NewAuthorizationRequest: %v", err)
	}
	if err := client.CompleteAuthorization(context.Background(), md, rec, req, "code-1", redirect); err != nil {
		t.Fatalf("CompleteAuthorization: %v", err)
	}
	assertExactForm(t, server, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"code-1"},
		"redirect_uri":  {redirect},
		"code_verifier": {req.Verifier},
		"resource":      {client.endpoint},
		"client_id":     {"cid-post"},
		"client_secret": {"shh"},
	})
	if auth := tokenAuthHeader(t, server); auth != "" {
		t.Errorf("client_secret_post sent Authorization header %q", auth)
	}
}

// tokenAuthHeader returns the recorded Authorization header value.
func tokenAuthHeader(t *testing.T, server *metadataServer) string {
	t.Helper()
	server.tokenReqMu.Lock()
	defer server.tokenReqMu.Unlock()
	if server.tokenReq == nil {
		return ""
	}
	return server.tokenReq.Header.Get("Authorization")
}

// seedMethodCredentials stores one client registration and refresh
// credential for an explicit auth method, issuer, and token endpoint.
func seedMethodCredentials(t *testing.T, secrets *fakeSecrets, clientID, clientSecret, authMethod, issuer, tokenEndpoint, refreshToken string) {
	t.Helper()
	rec := ClientRecord{ClientID: clientID, ClientSecret: clientSecret, AuthMethod: authMethod, Issuer: issuer, RegisteredAt: time.Now().UTC()}
	recData, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal client record: %v", err)
	}
	if err := secrets.SetSecret(labelClient, recData); err != nil {
		t.Fatalf("store client: %v", err)
	}
	cred := refreshCredential{RefreshToken: refreshToken, TokenEndpoint: tokenEndpoint, Issuer: issuer, Updated: time.Now().UTC()}
	credData, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("marshal credential: %v", err)
	}
	if err := secrets.SetSecret(labelRefresh, credData); err != nil {
		t.Fatalf("store credential: %v", err)
	}
}

// TestRefreshStaleProfileIssuer proves refresh is bound to the active
// profile issuer: when the stored client record and stored credential agree
// with each other but both predate a profile issuer change, refresh fails
// closed without sending the foreign token to the stored endpoint.
func TestRefreshStaleProfileIssuer(t *testing.T) {
	server := (&metadataServer{}).start(t)
	server.tokenBody = `{"access_token":"at-1","token_type":"Bearer","expires_in":3600}`
	secrets := newFakeSecrets()
	client := clientForServer(t, server, secrets, newFakeLease(), newTestClock(time.Unix(1_700_000_000, 0)))

	staleIssuer := "https://stale.example/oauth"
	seedMethodCredentials(t, secrets, "cid-1", "", "none", staleIssuer, "https://stale.example/oauth/token", "rt-foreign")

	if _, err := client.Refresh(context.Background()); err == nil {
		t.Fatal("stale-issuer refresh accepted")
	}
	if server.tokenCalls != 0 {
		t.Fatalf("token request sent for a foreign issuer: %d calls", server.tokenCalls)
	}
}

// TestRefreshStoredEndpointPolicy proves the stored token endpoint is
// re-validated against the issuer-bound metadata policy before use: the
// endpoint must be secure and on the issuer's origin.
func TestRefreshStoredEndpointPolicy(t *testing.T) {
	issuer := "https://tama.example/oauth"
	for _, tc := range []struct {
		name    string
		tokenEP string
	}{
		{"endpoint on a different origin", "https://other.example/token"},
		{"non-loopback http endpoint", "http://tama.example/oauth/token"},
		{"endpoint without a host", "https://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := (&metadataServer{}).start(t)
			secrets := newFakeSecrets()
			client := newStaticClient(t, secrets, newFakeLease(), newTestClock(time.Unix(1_700_000_000, 0)))
			seedMethodCredentials(t, secrets, "cid-1", "", "none", issuer, tc.tokenEP, "rt-1")

			if _, err := client.Refresh(context.Background()); err == nil {
				t.Fatal("stored endpoint accepted")
			}
			if server.tokenCalls != 0 {
				t.Fatalf("token request sent for an unvalidated endpoint: %d calls", server.tokenCalls)
			}
		})
	}
}
