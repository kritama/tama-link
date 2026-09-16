package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAuthorizationRequest(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(ts.Close)
	secrets := newFakeSecrets()
	client := newStaticClient(t, secrets, newFakeLease(), newTestClock(time.Now()))
	rec := &ClientRecord{ClientID: "cid-1", AuthMethod: "client_secret_basic", Issuer: testIssuer}
	md := serverMetadata(testBase)

	redirect := "http://127.0.0.1:51234/callback"
	req, err := client.NewAuthorizationRequest(md, rec, redirect)
	if err != nil {
		t.Fatalf("NewAuthorizationRequest: %v", err)
	}
	q := req.URL.Query()
	if got := q.Get("response_type"); got != "code" {
		t.Errorf("response_type = %q", got)
	}
	if got := q.Get("client_id"); got != "cid-1" {
		t.Errorf("client_id = %q", got)
	}
	if got := q.Get("redirect_uri"); got != redirect {
		t.Errorf("redirect_uri = %q, want the exact listener uri %q", got, redirect)
	}
	if req.RedirectURI != redirect {
		t.Errorf("request redirect uri = %q, want %q", req.RedirectURI, redirect)
	}
	if q.Get("state") == "" || q.Get("state") != req.State {
		t.Errorf("state mismatch: %q vs %q", q.Get("state"), req.State)
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q", got)
	}
	sum := sha256.Sum256([]byte(req.Verifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := q.Get("code_challenge"); got != wantChallenge {
		t.Errorf("code_challenge = %q, want S256(verifier)", got)
	}
	if got := q.Get("resource"); got != testEndpoint {
		t.Errorf("resource = %q, want exact endpoint", got)
	}
	if req.URL.Host != "tama.example" {
		t.Errorf("authorization host = %q", req.URL.Host)
	}
}

func TestCompleteAuthorization(t *testing.T) {
	server := (&metadataServer{}).start(t)
	server.tokenBody = `{"access_token":"at-1","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-1"}`
	secrets := newFakeSecrets()
	clock := newTestClock(time.Unix(1_700_000_000, 0))
	lease := newFakeLease()
	client := clientForServer(t, server, secrets, lease, clock)
	rec := &ClientRecord{ClientID: "cid-1", ClientSecret: "shh", AuthMethod: "client_secret_basic", Issuer: client.issuer}
	md := serverMetadata(server.ts.URL)

	// The listener port is selected before the authorization URL exists; the
	// exact URI then flows through both the request and the exchange.
	observed := "http://127.0.0.1:51234/callback"
	authReq, err := client.NewAuthorizationRequest(md, rec, observed)
	if err != nil {
		t.Fatalf("NewAuthorizationRequest: %v", err)
	}
	if err := client.CompleteAuthorization(context.Background(), md, rec, authReq, "code-1", observed); err != nil {
		t.Fatalf("CompleteAuthorization: %v", err)
	}

	// Wire assertions.
	server.tokenReqMu.Lock()
	tr := server.tokenReq
	raw := make([]byte, len(server.tokenRaw))
	copy(raw, server.tokenRaw)
	server.tokenReqMu.Unlock()
	form, err := url.ParseQuery(string(raw))
	if err != nil {
		t.Fatalf("parse token form: %v", err)
	}
	if form.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", form.Get("grant_type"))
	}
	if form.Get("code") != "code-1" {
		t.Errorf("code = %q", form.Get("code"))
	}
	if form.Get("code_verifier") != authReq.Verifier {
		t.Error("code_verifier does not match the request verifier")
	}
	if form.Get("redirect_uri") != observed {
		t.Errorf("redirect_uri = %q, want observed %q", form.Get("redirect_uri"), observed)
	}
	if form.Get("resource") != client.endpoint {
		t.Errorf("resource = %q", form.Get("resource"))
	}
	user, pass, ok := tr.BasicAuth()
	if !ok || user != "cid-1" || pass != "shh" {
		t.Errorf("basic auth = %q/%q ok=%v", user, pass, ok)
	}

	// Durable state: refresh credential persisted, access token in memory.
	tok, err := client.Token(context.Background())
	if err != nil || tok != "at-1" {
		t.Fatalf("Token = %q err=%v", tok, err)
	}
	credData := []byte(liveCredential(t, lease, secrets))
	var cred refreshCredential
	if err := json.Unmarshal(credData, &cred); err != nil {
		t.Fatalf("decode credential: %v", err)
	}
	if cred.RefreshToken != "rt-1" || cred.Issuer != client.issuer || cred.TokenEndpoint != md.AS.TokenEndpoint {
		t.Errorf("credential = %+v", cred)
	}
	if !strings.Contains(string(credData), "rt-1") {
		t.Error("credential did not persist the refresh token")
	}
}

func TestCompleteAuthorizationRejections(t *testing.T) {
	server := (&metadataServer{}).start(t)
	secrets := newFakeSecrets()
	clock := newTestClock(time.Now())
	client := clientForServer(t, server, secrets, newFakeLease(), clock)
	rec := &ClientRecord{ClientID: "cid-1", ClientSecret: "shh", AuthMethod: "client_secret_basic", Issuer: client.issuer}
	md := serverMetadata(server.ts.URL)
	requestURI := "http://127.0.0.1:51234/callback"
	authReq, err := client.NewAuthorizationRequest(md, rec, requestURI)
	if err != nil {
		t.Fatalf("NewAuthorizationRequest: %v", err)
	}
	server.tokenBody = `{"access_token":"at-1","token_type":"Bearer","expires_in":60,"refresh_token":"rt-1"}`

	t.Run("non-loopback request redirect", func(t *testing.T) {
		if _, err := client.NewAuthorizationRequest(md, rec, "https://evil.example/cb"); err == nil {
			t.Fatal("non-loopback request redirect accepted")
		}
	})

	t.Run("mismatched observed port is rejected before the token request", func(t *testing.T) {
		server.tokenCalls = 0
		err := client.CompleteAuthorization(context.Background(), md, rec, authReq, "code", "http://127.0.0.1:61234/callback")
		if err == nil {
			t.Fatal("mismatched port accepted")
		}
		if server.tokenCalls != 0 {
			t.Fatalf("token request sent for a mismatched redirect uri: %d calls", server.tokenCalls)
		}
	})

	t.Run("mismatched observed path is rejected before the token request", func(t *testing.T) {
		server.tokenCalls = 0
		err := client.CompleteAuthorization(context.Background(), md, rec, authReq, "code", "http://127.0.0.1:51234/other")
		if err == nil {
			t.Fatal("mismatched path accepted")
		}
		if server.tokenCalls != 0 {
			t.Fatalf("token request sent for a mismatched redirect uri: %d calls", server.tokenCalls)
		}
	})

	t.Run("invalid grant", func(t *testing.T) {
		server.tokenBody = `{"error":"invalid_grant","error_description":"code already used"}`
		err := client.CompleteAuthorization(context.Background(), md, rec, authReq, "code", requestURI)
		if !errors.Is(err, ErrGrantInvalid) {
			t.Fatalf("err = %v, want ErrGrantInvalid", err)
		}
		if strings.Contains(err.Error(), "code already used") {
			t.Error("provider error description leaked into the error")
		}
	})

	t.Run("no refresh token", func(t *testing.T) {
		server.tokenBody = `{"access_token":"at-1","token_type":"Bearer","expires_in":60}`
		err := client.CompleteAuthorization(context.Background(), md, rec, authReq, "code", requestURI)
		if !errors.Is(err, ErrNoCredentials) {
			t.Fatalf("err = %v, want ErrNoCredentials", err)
		}
	})

	t.Run("empty code", func(t *testing.T) {
		if err := client.CompleteAuthorization(context.Background(), md, rec, authReq, "", requestURI); err == nil {
			t.Fatal("empty code accepted")
		}
	})
}
