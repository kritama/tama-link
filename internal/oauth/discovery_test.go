package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWellKnownURLs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"root path", "https://tama.example", "https://tama.example/.well-known/oauth-protected-resource"},
		{"endpoint path", "https://tama.example/mcp/app", "https://tama.example/.well-known/oauth-protected-resource/mcp/app"},
		{"nested path", "https://tama.example/a/b", "https://tama.example/.well-known/oauth-protected-resource/a/b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := prmURL(tc.in)
			if err != nil {
				t.Fatalf("prmURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("prmURL = %q, want %q", got, tc.want)
			}
		})
	}
	asCases := []struct {
		name string
		in   string
		want string
	}{
		{"origin only", "https://tama.example", "https://tama.example/.well-known/oauth-authorization-server"},
		{"with path", "https://tama.example/oauth", "https://tama.example/.well-known/oauth-authorization-server/oauth"},
		{"trailing slash", "https://tama.example/oauth/", "https://tama.example/.well-known/oauth-authorization-server/oauth"},
	}
	for _, tc := range asCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := asMetadataURL(tc.in)
			if err != nil {
				t.Fatalf("asMetadataURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("asMetadataURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// startedServer serves a valid metadata fixture pair. The client is built
// before the documents are re-pointed, which is safe because discovery
// reads endpoint and issuer from the client, not the documents.
func startedServer(t *testing.T) (*metadataServer, *Client) {
	t.Helper()
	server := (&metadataServer{}).start(t)
	server.prm = serverPRM(server.ts.URL)
	server.as = serverAS(server.ts.URL)
	client := clientForServer(t, server, newFakeSecrets(), newFakeLease(), newTestClock(time.Now()))
	return server, client
}

func TestDiscoverHappy(t *testing.T) {
	_, client := startedServer(t)
	md, err := client.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if md.PRM.Resource != client.endpoint {
		t.Errorf("PRM resource = %q, want %q", md.PRM.Resource, client.endpoint)
	}
	if md.AS.Issuer != client.issuer {
		t.Errorf("AS issuer = %q, want %q", md.AS.Issuer, client.issuer)
	}
	if md.ASURL != client.issuer {
		t.Errorf("ASURL = %q, want %q", md.ASURL, client.issuer)
	}
}

func TestDiscoverValidation(t *testing.T) {
	cases := []struct {
		name    string
		prm     func(serverURL string) string
		as      func(serverURL string) string
		ok      bool
		wantErr string
	}{
		{
			name: "prm issuer is the full endpoint (allowed)",
			prm: func(s string) string {
				return fmt.Sprintf(`{"issuer":%q,"authorization_servers":[%q],"resource":%q}`, s+"/mcp/app", s+"/oauth", s+"/mcp/app")
			},
			as: serverAS,
			ok: true,
		},
		{
			name: "resource does not bind endpoint",
			prm: func(s string) string {
				return fmt.Sprintf(`{"authorization_servers":[%q],"resource":%q}`, s+"/oauth", s+"/mcp/other")
			},
			as:      serverAS,
			wantErr: "does not bind",
		},
		{
			name:    "no authorization servers",
			prm:     func(s string) string { return fmt.Sprintf(`{"authorization_servers":[],"resource":%q}`, s+"/mcp/app") },
			as:      serverAS,
			wantErr: "no authorization servers",
		},
		{
			name: "issuer does not match expected",
			prm:  serverPRM,
			as: func(s string) string {
				doc := serverAS(s)
				return strings.Replace(doc, `"issuer": `+quote(s+"/oauth"), `"issuer": `+quote(s+"/other-issuer"), 1)
			},
			wantErr: "issuer does not match",
		},
		{
			name: "no matching authorization server",
			prm: func(s string) string {
				return fmt.Sprintf(`{"authorization_servers":[%q],"resource":%q}`, s+"/not-the-issuer", s+"/mcp/app")
			},
			as:      serverAS,
			wantErr: "no authorization server matches",
		},
		{
			name: "missing S256",
			prm:  serverPRM,
			as: func(s string) string {
				return strings.Replace(serverAS(s), `["S256"]`, `["plain"]`, 1)
			},
			wantErr: "PKCE S256",
		},
		{
			name: "missing refresh grant",
			prm:  serverPRM,
			as: func(s string) string {
				return strings.Replace(serverAS(s), `["authorization_code", "refresh_token"]`, `["authorization_code"]`, 1)
			},
			wantErr: "required grants",
		},
		{
			name: "missing registration endpoint",
			prm:  serverPRM,
			as: func(s string) string {
				return strings.Replace(serverAS(s), fmt.Sprintf(`"registration_endpoint": %q,`, s+"/oauth/register"), "", 1)
			},
			wantErr: "registration endpoint",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ts *httptest.Server
			ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/.well-known/oauth-protected-resource/mcp/app":
					_, _ = fmt.Fprint(w, tc.prm(ts.URL))
				case "/.well-known/oauth-authorization-server/oauth":
					_, _ = fmt.Fprint(w, tc.as(ts.URL))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(ts.Close)
			client, err := New(Config{
				Endpoint: ts.URL + "/mcp/app",
				Issuer:   ts.URL + "/oauth",
				Secrets:  newFakeSecrets(),
				Lease:    newFakeLease(),
				Clock:    newTestClock(time.Now()).nowFn(),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = client.Discover(context.Background())
			if tc.ok {
				if err != nil {
					t.Fatalf("validation rejected a valid document: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validation accepted: want %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want containing %q", err, tc.wantErr)
			}
			if !errors.Is(err, ErrMetadata) {
				t.Errorf("err = %v, want ErrMetadata wrap", err)
			}
		})
	}
}

func TestDiscoverMetadataUnavailable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	client, err := New(Config{
		Endpoint: ts.URL + "/mcp/app",
		Issuer:   ts.URL + "/oauth",
		Secrets:  newFakeSecrets(),
		Lease:    newFakeLease(),
		Clock:    newTestClock(time.Now()).nowFn(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Discover(context.Background()); !errors.Is(err, ErrMetadata) {
		t.Fatalf("err = %v, want ErrMetadata", err)
	}
}

func TestNewEndpointPolicy(t *testing.T) {
	if _, err := New(Config{
		Endpoint: "https://tama.example/mcp/app",
		Issuer:   testIssuer,
		Secrets:  newFakeSecrets(),
		Lease:    newFakeLease(),
		Clock:    newTestClock(time.Now()).nowFn(),
	}); err != nil {
		t.Fatalf("https endpoint rejected: %v", err)
	}
	if _, err := New(Config{
		Endpoint: "http://127.0.0.1:8080/mcp/app",
		Issuer:   "http://127.0.0.1:8080/oauth",
		Secrets:  newFakeSecrets(),
		Lease:    newFakeLease(),
		Clock:    newTestClock(time.Now()).nowFn(),
	}); err != nil {
		t.Fatalf("loopback http endpoint rejected: %v", err)
	}
	if _, err := New(Config{
		Endpoint: "http://tama.example/mcp/app",
		Issuer:   "http://tama.example/oauth",
		Secrets:  newFakeSecrets(),
		Lease:    newFakeLease(),
		Clock:    newTestClock(time.Now()).nowFn(),
	}); err == nil {
		t.Fatal("non-loopback http endpoint accepted")
	}
	if _, err := New(Config{
		Endpoint:    testEndpoint,
		Issuer:      testIssuer,
		Secrets:     newFakeSecrets(),
		Lease:       newFakeLease(),
		RedirectURI: "https://127.0.0.1",
		Clock:       newTestClock(time.Now()).nowFn(),
	}); err == nil {
		t.Fatal("non-loopback-scheme redirect uri accepted")
	}
}

// quote renders s as a JSON string literal.
func quote(s string) string {
	return `"` + s + `"`
}
