package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/upstream"
)

type fakeSession struct {
	ready      bool
	token      string
	authorized int
	loggedOut  int
	dbPath     string
}

func (f *fakeSession) Close() error { return nil }
func (f *fakeSession) Authorize(context.Context, bool) error {
	f.authorized++
	f.ready = true
	f.token = "memory-token"
	return nil
}
func (f *fakeSession) Token(context.Context) (string, error) {
	if f.token == "" {
		return "", errors.New("no token")
	}
	return f.token, nil
}
func (f *fakeSession) Ready(context.Context) (bool, error) { return f.ready, nil }
func (f *fakeSession) Logout(context.Context) error {
	f.loggedOut++
	f.ready = false
	f.token = ""
	return nil
}
func (f *fakeSession) ClaimLease(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (f *fakeSession) RenewLease(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (f *fakeSession) ReleaseLease(context.Context, string, string) error { return nil }
func (f *fakeSession) LeaseGeneration(context.Context, string, string) (int64, bool, error) {
	return 1, true, nil
}
func (f *fakeSession) CommitLease(context.Context, string, string, int64) (bool, error) {
	return true, nil
}
func (f *fakeSession) DatabasePath() string { return f.dbPath }

func privateConfig(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestNonInteractiveCreatePublishesTemplateProfile(t *testing.T) {
	t.Parallel()
	configDir := privateConfig(t)
	srv := metadataServer(t, []string{"mcp.message"})
	tmpl, err := tama2026.Lookup(profile.KindApp)
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{dbPath: filepath.Join(t.TempDir(), "state.db")}
	var sawToken string
	svc := testService(t, configDir, srv.Client(), session, func(_ context.Context, endpoint, token string) (*upstream.DiscoverResult, []*upstream.LiveTool, error) {
		if endpoint != srv.URL+"/mcp/app" {
			t.Errorf("endpoint = %s", endpoint)
		}
		sawToken = token
		return discoverResult(tmpl), liveFrom(tmpl), nil
	})
	result, err := svc.Run(context.Background(), Request{
		Address: srv.URL, AddressSet: true,
		Type: string(profile.KindApp), TypeSet: true,
		Profile: "tama-app", ProfileSet: true,
		Issuer: srv.URL, IssuerSet: true,
		Yes: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Created || result.Name != "tama-app" || !strings.Contains(result.ServeCommand, "serve --profile tama-app") {
		t.Fatalf("result = %+v", result)
	}
	if sawToken != "memory-token" {
		t.Fatalf("catalog token = %q", sawToken)
	}
	loaded, err := profile.Load("tama-app", configDir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Issuer != srv.URL || len(loaded.Operations) != 1 || loaded.Operations[0].Name != "message" {
		t.Fatalf("loaded = %+v", loaded)
	}
	if _, err := loadJournal(configDir, "tama-app"); !os.IsNotExist(err) {
		t.Fatalf("journal remained: %v", err)
	}
}

func TestServeCommandPreservesPathSeparators(t *testing.T) {
	t.Parallel()
	path := "C:\\Users\\me\\my cfg"
	got := serveCommand("tama-app", path, true)
	if strings.Contains(got, "\\\\") {
		t.Fatalf("doubled backslashes: %s", got)
	}
	if !strings.Contains(got, path) {
		t.Fatalf("path not preserved: %s", got)
	}
	quoted := quoteWindowsArg("C:\\Users\\me\\cfg\"x")
	if !strings.Contains(quoted, "C:\\Users\\me\\cfg") || !strings.Contains(quoted, "\\\"") {
		t.Fatalf("windows quote = %s", quoted)
	}
}

func TestContradictNormalizesResumeInputs(t *testing.T) {
	t.Parallel()
	record := journal{
		Name: "tama-app", Origin: "https://tama.example", Endpoint: "https://tama.example/mcp/app",
		Issuer: "https://auth.example", Template: "app",
	}
	if err := contradict(Request{Address: "https://tama.example/", AddressSet: true, Issuer: "https://auth.example/", IssuerSet: true, Type: "app", TypeSet: true}, record); err != nil {
		t.Fatalf("normalized resume inputs: %v", err)
	}
	if err := contradict(Request{Type: "system", TypeSet: true}, record); err == nil {
		t.Fatal("accepted a conflicting type")
	}
}

func TestNonInteractiveMissingAddressDoesNotPrompt(t *testing.T) {
	t.Parallel()
	svc := testService(t, privateConfig(t), nil, &fakeSession{}, nil)
	_, err := svc.Run(context.Background(), Request{})
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Status != StatusUsage || !strings.Contains(err.Error(), "--profile") {
		t.Fatalf("error = %v", err)
	}
}

func TestCatalogFailureKeepsJournal(t *testing.T) {
	t.Parallel()
	configDir := privateConfig(t)
	srv := metadataServer(t, []string{"mcp.message"})
	session := &fakeSession{}
	svc := testService(t, configDir, srv.Client(), session, func(context.Context, string, string) (*upstream.DiscoverResult, []*upstream.LiveTool, error) {
		return nil, nil, errors.New("catalog unavailable")
	})
	_, err := svc.Run(context.Background(), Request{
		Address: srv.URL, AddressSet: true,
		Profile: "tama-app", ProfileSet: true,
		Issuer: srv.URL, IssuerSet: true,
		Yes: true,
	})
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("error = %v, want incomplete", err)
	}
	if _, statErr := profile.Load("tama-app", configDir); !errors.Is(statErr, profile.ErrNotFound) {
		t.Fatalf("profile published on failure: %v", statErr)
	}
	record, err := loadJournal(configDir, "tama-app")
	if err != nil {
		t.Fatal(err)
	}
	if record.Stage != stageAuthorized || strings.Contains(string(mustJournal(record)), "memory-token") {
		t.Fatalf("journal = %+v", record)
	}
}

func testService(t *testing.T, configDir string, client *http.Client, session *fakeSession, catalog func(context.Context, string, string) (*upstream.DiscoverResult, []*upstream.LiveTool, error)) *Service {
	t.Helper()
	if catalog == nil {
		catalog = func(context.Context, string, string) (*upstream.DiscoverResult, []*upstream.LiveTool, error) {
			t.Fatal("catalog should not be called")
			return nil, nil, nil
		}
	}
	svc, err := New(Options{
		ConfigDir:   configDir,
		Interactive: false,
		Stdin:       strings.NewReader(""),
		Stderr:      ioDiscard{},
		HTTP:        client,
		OpenSession: func(context.Context, *profile.Profile) (Session, error) { return session, nil },
		ReadCatalog: catalog,
		LoginExisting: func(context.Context, *profile.Profile) error {
			return errors.New("existing login")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func metadataServer(t *testing.T, scopes []string) *httptest.Server {
	t.Helper()
	scopeJSON, err := json.Marshal(scopes)
	if err != nil {
		t.Fatal(err)
	}
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource/mcp/app":
			_, _ = w.Write([]byte(`{"resource":"` + srv.URL + `/mcp/app","authorization_servers":["` + srv.URL + `"],"scopes_supported":` + string(scopeJSON) + `}`))
		case "/.well-known/oauth-authorization-server":
			_, _ = w.Write([]byte(`{"issuer":"` + srv.URL + `","authorization_endpoint":"` + srv.URL + `/authorize","token_endpoint":"` + srv.URL + `/token","registration_endpoint":"` + srv.URL + `/register","code_challenge_methods_supported":["S256"],"grant_types_supported":["authorization_code","refresh_token"],"token_endpoint_auth_methods_supported":["none"],"scopes_supported":` + string(scopeJSON) + `}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func discoverResult(tmpl tama2026.Template) *upstream.DiscoverResult {
	return &upstream.DiscoverResult{
		SupportedVersions: []string{"2026-07-28"},
		Capabilities:      json.RawMessage(`{"tools":{},"extensions":{"io.modelcontextprotocol/tasks":{}}}`),
		Instructions:      tmpl.Instructions,
	}
}

func liveFrom(tmpl tama2026.Template) []*upstream.LiveTool {
	out := make([]*upstream.LiveTool, 0, len(tmpl.Operations)+1)
	for _, op := range tmpl.Operations {
		annotations, _ := json.Marshal(op.Annotations)
		out = append(out, &upstream.LiveTool{
			Name: op.Name, InputSchema: op.InputSchema, OutputSchema: op.OutputSchema, Annotations: annotations,
		})
	}
	out = append(out, &upstream.LiveTool{Name: "extra.tool", InputSchema: json.RawMessage(`{"type":"object"}`)})
	return out
}

func mustJournal(j journal) []byte {
	data, err := encodeJournal(j)
	if err != nil {
		panic(err)
	}
	return data
}
