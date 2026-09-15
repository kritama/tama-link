package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeSecrets is an in-memory SecretStore for tests.
type fakeSecrets struct {
	mu    sync.Mutex
	items map[string][]byte
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{items: map[string][]byte{}}
}

func (f *fakeSecrets) GetSecret(label string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.items[label]
	if !ok {
		return nil, false, nil
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, true, nil
}

func (f *fakeSecrets) SetSecret(label string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]byte, len(data))
	copy(out, data)
	f.items[label] = out
	return nil
}

func (f *fakeSecrets) DeleteSecret(label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, label)
	return nil
}

// fakeLease is an in-memory Leaser. holdOther simulates another process
// holding the lease; onClaim runs just before a claim succeeds, letting a
// test write a replacement credential while the claimant waits.
type fakeLease struct {
	mu       sync.Mutex
	holder   string
	onClaim  func()
	claims   int
	released int
}

func newFakeLease() *fakeLease { return &fakeLease{} }

func (f *fakeLease) holdOther() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holder = "other-process"
}

func (f *fakeLease) ClaimLease(_ context.Context, name, owner string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	if name != refreshLeaseName || ttl <= 0 {
		f.mu.Unlock()
		return false, fmt.Errorf("unexpected lease name %q ttl %s", name, ttl)
	}
	if f.holder != "" && f.holder != owner {
		f.claims++
		f.mu.Unlock()
		return false, nil
	}
	f.claims++
	hook := f.onClaim
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holder = owner
	return true, nil
}

func (f *fakeLease) ReleaseLease(_ context.Context, name, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != refreshLeaseName {
		return fmt.Errorf("unexpected lease name %q", name)
	}
	if f.holder == owner {
		f.holder = ""
		f.released++
	}
	return nil
}

// testClock is a mutable clock for deterministic expiry tests.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(start time.Time) *testClock {
	return &testClock{now: start}
}

func (c *testClock) nowFn() func() time.Time {
	return func() time.Time {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.now
	}
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// testBase is the canonical profile endpoint used by static tests.
const (
	testBase     = "https://tama.example"
	testEndpoint = testBase + "/mcp/app"
	testIssuer   = testBase + "/oauth"
)

// newStaticClient builds an OAuth client for the canonical static profile.
func newStaticClient(t *testing.T, secrets *fakeSecrets, lease *fakeLease, clock *testClock) *Client {
	t.Helper()
	client, err := New(Config{
		Endpoint:   testEndpoint,
		Issuer:     testIssuer,
		Secrets:    secrets,
		Lease:      lease,
		Clock:      clock.nowFn(),
		HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

// metadataServer serves protected-resource and authorization-server metadata
// plus a token endpoint for one fixture pair.
type metadataServer struct {
	t          *testing.T
	ts         *httptest.Server
	prm        string
	as         string
	tokenBody  string
	tokenReq   *http.Request
	tokenRaw   []byte
	tokenCalls int
	tokenReqMu sync.Mutex
}

func (s *metadataServer) start(t *testing.T) *metadataServer {
	t.Helper()
	s.t = t
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp/app":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, s.prm)
		case "/.well-known/oauth-authorization-server/oauth":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, s.as)
		case "/oauth/token":
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read token body: %v", err)
			}
			s.tokenReqMu.Lock()
			s.tokenReq = r.Clone(context.Background())
			s.tokenRaw = raw
			s.tokenCalls++
			s.tokenReqMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, s.tokenBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.ts.Close)
	return s
}

// clientForServer builds the test client against one metadata server so the
// endpoint and issuer point at the fixture.
func clientForServer(t *testing.T, server *metadataServer, secrets *fakeSecrets, lease *fakeLease, clock *testClock) *Client {
	t.Helper()
	client, err := New(Config{
		Endpoint:   server.ts.URL + "/mcp/app",
		Issuer:     server.ts.URL + "/oauth",
		Secrets:    secrets,
		Lease:      lease,
		Clock:      clock.nowFn(),
		HTTPClient: &http.Client{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

// serverMetadata is a validated Metadata value for one test server, used
// directly by exchange and refresh tests.
func serverMetadata(base string) *Metadata {
	return &Metadata{
		PRM: ProtectedResource{
			Issuer:               base,
			AuthorizationServers: []string{base + "/oauth"},
			Resource:             base + "/mcp/app",
		},
		AS: AuthorizationServer{
			Issuer:                   base + "/oauth",
			AuthorizationEndpoint:    base + "/oauth/authorize",
			TokenEndpoint:            base + "/oauth/token",
			RegistrationEndpoint:     base + "/oauth/register",
			CodeChallengeMethods:     []string{"S256"},
			GrantTypes:               []string{"authorization_code", "refresh_token"},
			TokenEndpointAuthMethods: []string{"client_secret_basic"},
		},
		ASURL: base + "/oauth",
	}
}

// seedCredentials stores one client registration and one refresh credential
// so exchange and refresh tests can start from a logged-in profile.
func seedCredentials(t *testing.T, secrets *fakeSecrets, clientID, clientSecret, tokenEndpoint, issuer, refreshToken string) {
	t.Helper()
	rec := ClientRecord{ClientID: clientID, ClientSecret: clientSecret, AuthMethod: "client_secret_basic", Issuer: issuer, RegisteredAt: time.Now().UTC()}
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

// serverPRM is a valid protected-resource document for one server.
func serverPRM(serverURL string) string {
	endpoint := serverURL + "/mcp/app"
	return fmt.Sprintf(`{"issuer":%q,"authorization_servers":[%q],"resource":%q}`, serverURL, serverURL+"/oauth", endpoint)
}

// serverAS is a valid authorization-server document for one server.
func serverAS(serverURL string) string {
	issuer := serverURL + "/oauth"
	return fmt.Sprintf(`{
		"issuer": %q,
		"authorization_endpoint": %q,
		"token_endpoint": %q,
		"registration_endpoint": %q,
		"code_challenge_methods_supported": ["S256"],
		"grant_types_supported": ["authorization_code", "refresh_token"],
		"response_types_supported": ["code"],
		"token_endpoint_auth_methods_supported": ["client_secret_basic"]
	}`, issuer, serverURL+"/oauth/authorize", serverURL+"/oauth/token", serverURL+"/oauth/register")
}
