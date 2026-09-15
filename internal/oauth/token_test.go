package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// tokenFixture wires one server, client, and logged-in profile together.
func tokenFixture(t *testing.T, refreshToken string) (*metadataServer, *Client, *fakeSecrets, *fakeLease, *testClock) {
	t.Helper()
	server := (&metadataServer{}).start(t)
	server.tokenBody = `{"access_token":"at-1","token_type":"Bearer","expires_in":3600,"refresh_token":"` + refreshToken + `"}`
	secrets := newFakeSecrets()
	lease := newFakeLease()
	clock := newTestClock(time.Unix(1_700_000_000, 0))
	client := clientForServer(t, server, secrets, lease, clock)
	seedCredentials(t, secrets, "cid-1", "shh", server.ts.URL+"/oauth/token", client.issuer, refreshToken)
	return server, client, secrets, lease, clock
}

func TestTokenCachesValid(t *testing.T) {
	server, client, _, _, clock := tokenFixture(t, "rt-1")
	tok1, err := client.Token(context.Background())
	if err != nil || tok1 != "at-1" {
		t.Fatalf("Token = %q err=%v", tok1, err)
	}
	// Advance within the recorded expiry: the token must be served from
	// memory with no second token request.
	clock.set(clock.now.Add(time.Minute))
	tok2, err := client.Token(context.Background())
	if err != nil || tok2 != "at-1" {
		t.Fatalf("cached Token = %q err=%v", tok2, err)
	}
	server.tokenReqMu.Lock()
	calls := server.tokenCalls
	server.tokenReqMu.Unlock()
	if calls != 1 {
		t.Errorf("token requests = %d, want 1 (cache hit)", calls)
	}
	expiry, ok := client.Expiry()
	if !ok || expiry.IsZero() {
		t.Fatalf("Expiry = %v ok=%v", expiry, ok)
	}
}

func TestTokenRefreshOnExpiry(t *testing.T) {
	server, client, secrets, lease, clock := tokenFixture(t, "rt-1")
	ctx := context.Background()

	if _, err := client.Token(ctx); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	clock.set(clock.now.Add(3600 * time.Second)) // past expiry

	tok, err := client.Token(ctx)
	if err != nil || tok != "at-1" {
		t.Fatalf("Token after expiry = %q err=%v", tok, err)
	}
	server.tokenReqMu.Lock()
	raw := make([]byte, len(server.tokenRaw))
	copy(raw, server.tokenRaw)
	server.tokenReqMu.Unlock()
	form := strings.Split(string(raw), "&")
	joined := strings.Join(form, " ")
	if !strings.Contains(joined, "grant_type=refresh_token") || !strings.Contains(joined, "refresh_token=rt-1") {
		t.Errorf("refresh wire = %q", joined)
	}
	if !strings.Contains(joined, "resource=") {
		t.Error("refresh wire carries no resource binding")
	}
	// Stable refresh: the same refresh token is retained.
	data, found, err := secrets.GetSecret(labelRefresh)
	if err != nil || !found {
		t.Fatalf("credential = %v", err)
	}
	if !strings.Contains(string(data), "rt-1") {
		t.Errorf("stable refresh token lost: %s", data)
	}
	if lease.released != 2 {
		t.Errorf("lease releases = %d, want 2", lease.released)
	}
}

func TestTokenReplacementRefresh(t *testing.T) {
	server, client, secrets, _, _ := tokenFixture(t, "rt-1")
	server.tokenBody = `{"access_token":"at-2","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2"}`

	if _, err := client.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	data, found, err := secrets.GetSecret(labelRefresh)
	if err != nil || !found {
		t.Fatalf("credential = %v", err)
	}
	if !strings.Contains(string(data), "rt-2") || strings.Contains(string(data), "rt-1") {
		t.Errorf("replacement refresh token not stored atomically: %s", data)
	}
}

func TestRefreshInvalidGrant(t *testing.T) {
	server, client, _, _, _ := tokenFixture(t, "rt-1")
	server.tokenBody = `{"error":"invalid_grant"}`

	_, err := client.Token(context.Background())
	if !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("err = %v, want ErrGrantInvalid", err)
	}
	if _, ok := client.Expiry(); ok {
		t.Error("in-memory token survived an invalid grant")
	}
}

func TestRefreshLeaseContention(t *testing.T) {
	_, client, _, lease, _ := tokenFixture(t, "rt-1")
	lease.holdOther()

	start := time.Now()
	_, err := client.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refresh lease") {
		t.Fatalf("err = %v, want lease contention", err)
	}
	if lease.claims != claimAttempts {
		t.Errorf("claims = %d, want bounded %d", lease.claims, claimAttempts)
	}
	if lease.released != 0 {
		t.Errorf("released = %d, want 0", lease.released)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("contention wait = %s, unbounded", elapsed)
	}
}

func TestRefreshRereadAdoptsReplacement(t *testing.T) {
	server, client, secrets, lease, _ := tokenFixture(t, "rt-1")
	// While the claimant holds the lease, a concurrent writer replaces the
	// stored credential; the claimant must re-read and use rt-2.
	lease.onClaim = func() {
		seedCredentials(t, secrets, "cid-1", "shh", server.ts.URL+"/oauth/token", client.issuer, "rt-2")
	}
	server.tokenBody = `{"access_token":"at-1","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2"}`

	if _, err := client.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	server.tokenReqMu.Lock()
	raw := make([]byte, len(server.tokenRaw))
	copy(raw, server.tokenRaw)
	server.tokenReqMu.Unlock()
	if !strings.Contains(string(raw), "refresh_token=rt-2") {
		t.Errorf("exchange used a stale refresh token: %s", raw)
	}
}

func TestRefreshIssuerMismatch(t *testing.T) {
	_, client, secrets, _, _ := tokenFixture(t, "rt-1")
	// Rebind the client record to a different issuer.
	rec := ClientRecord{ClientID: "cid-1", AuthMethod: "client_secret_basic", Issuer: "https://foreign.example/oauth"}
	if err := secrets.SetSecret(labelClient, mustJSON(t, rec)); err != nil {
		t.Fatalf("set client: %v", err)
	}
	_, err := client.Token(context.Background())
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
}

func TestHasCredentialsAndLogout(t *testing.T) {
	_, client, secrets, _, _ := tokenFixture(t, "rt-1")
	if ok, err := client.HasCredentials(); err != nil || !ok {
		t.Fatalf("HasCredentials = %v %v", ok, err)
	}
	if _, err := client.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if err := client.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if ok, _ := client.HasCredentials(); ok {
		t.Error("credentials survived logout")
	}
	if _, ok := client.Expiry(); ok {
		t.Error("in-memory token survived logout")
	}
	if _, found, _ := secrets.GetSecret(labelClient); found {
		t.Error("client record survived logout")
	}
}

func TestRefreshCancellation(t *testing.T) {
	_, client, _, lease, _ := tokenFixture(t, "rt-1")
	lease.holdOther()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestNewValidation(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
	}{
		{"empty endpoint", func(c *Config) { c.Endpoint = "" }},
		{"empty issuer", func(c *Config) { c.Issuer = "" }},
		{"nil secrets", func(c *Config) { c.Secrets = nil }},
		{"nil lease", func(c *Config) { c.Lease = nil }},
		{"bad redirect", func(c *Config) { c.RedirectURI = "https://127.0.0.1" }},
		{"foreign redirect", func(c *Config) { c.RedirectURI = "http://example.com/cb" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				Endpoint: testEndpoint,
				Issuer:   testIssuer,
				Secrets:  newFakeSecrets(),
				Lease:    newFakeLease(),
				Clock:    newTestClock(time.Now()).nowFn(),
			}
			tc.mut(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("config accepted")
			}
		})
	}
}

// mustJSON is a test helper.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
