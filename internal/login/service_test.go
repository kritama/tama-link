package login

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/99designs/keyring"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/credential"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/oauth"
	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/store"
)

// memKeyring is an in-memory keyring.Keyring for tests, standing in for the
// platform credential store.
type memKeyring struct {
	mu    sync.Mutex
	items map[string]keyring.Item
}

func newMemKeyring() *memKeyring { return &memKeyring{items: map[string]keyring.Item{}} }

func (m *memKeyring) Get(key string) (keyring.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[key]
	if !ok {
		return keyring.Item{}, keyring.ErrKeyNotFound
	}
	return it, nil
}

func (m *memKeyring) GetMetadata(string) (keyring.Metadata, error) {
	return keyring.Metadata{}, keyring.ErrMetadataNotSupported
}

func (m *memKeyring) Set(item keyring.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[item.Key] = item
	return nil
}

func (m *memKeyring) Remove(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, key)
	return nil
}

func (m *memKeyring) Reset() error { return nil }

func (m *memKeyring) Keys() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.items))
	for key := range m.items {
		keys = append(keys, key)
	}
	return keys, nil
}

// oauthFixture is one loopback authorization server: protected-resource and
// authorization-server metadata, dynamic registration, and a token endpoint.
type oauthFixture struct {
	t  *testing.T
	ts *httptest.Server
	// prmScopes and asScopes are the scopes_supported JSON arrays, or
	// empty when the document omits the field.
	prmScopes string
	asScopes  string
	// advertiseIssuer makes the server declare RFC 9207 issuer support in
	// the standard boolean metadata member, which makes the callback
	// require the iss parameter.
	advertiseIssuer bool
	// regScope is the scope a registration declares; empty omits it.
	regScope string
	// tokenScope is the scope the token endpoint echoes; empty omits it.
	tokenScope string

	mu         sync.Mutex
	regBody    string
	regCalls   int
	tokenForm  url.Values
	tokenCalls int
}

func (f *oauthFixture) start(t *testing.T) *oauthFixture {
	t.Helper()
	f.t = t
	f.ts = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.ts.Close)
	return f
}

func (f *oauthFixture) base() string { return f.ts.URL }

func (f *oauthFixture) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/oauth-protected-resource/mcp/app":
		doc := map[string]any{
			"issuer":                f.base(),
			"authorization_servers": []string{f.base() + "/oauth"},
			"resource":              f.base() + "/mcp/app",
		}
		if v := f.field(f.prmScopes); v != nil {
			doc["scopes_supported"] = v
		}
		f.writeJSON(w, http.StatusOK, doc)
	case "/.well-known/oauth-authorization-server/oauth":
		doc := map[string]any{
			"issuer":                                f.base() + "/oauth",
			"authorization_endpoint":                f.base() + "/oauth/authorize",
			"token_endpoint":                        f.base() + "/oauth/token",
			"registration_endpoint":                 f.base() + "/oauth/register",
			"code_challenge_methods_supported":      []string{"S256"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"response_types_supported":              []string{"code"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
		}
		if v := f.field(f.asScopes); v != nil {
			doc["scopes_supported"] = v
		}
		if f.advertiseIssuer {
			doc["authorization_response_iss_parameter_supported"] = true
		}
		f.writeJSON(w, http.StatusOK, doc)
	case "/oauth/register":
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.regBody = string(raw)
		f.regCalls++
		f.mu.Unlock()
		resp := map[string]any{
			"client_id":                "cid-test",
			"client_secret":            "shh",
			"client_secret_expires_at": int64(0),
		}
		if f.regScope != "" {
			resp["scope"] = f.regScope
		}
		f.writeJSON(w, http.StatusCreated, resp)
	case "/oauth/token":
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.mu.Lock()
		f.tokenForm = form
		f.tokenCalls++
		f.mu.Unlock()
		resp := map[string]any{
			"access_token":  "at-1",
			"token_type":    "Bearer",
			"expires_in":    int64(3600),
			"refresh_token": fmt.Sprintf("rt-%d", f.tokenCalls),
		}
		if f.tokenScope != "" {
			resp["scope"] = f.tokenScope
		}
		f.writeJSON(w, http.StatusOK, resp)
	default:
		http.NotFound(w, r)
	}
}

// field decodes one JSON array for embedding, or returns nil to omit.
func (f *oauthFixture) field(raw string) any {
	if raw == "" {
		return nil
	}
	var v []string
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		f.t.Fatalf("fixture scope list %q: %v", raw, err)
	}
	return v
}

func (f *oauthFixture) writeJSON(w http.ResponseWriter, status int, doc any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(doc)
}

// consent is one fake browser: it records the authorization URL and drives
// the loopback callback exactly like a completed (or denied) consent page.
type consent struct {
	delay time.Duration
	// sendIssuer echoes the authorization server's issuer in iss.
	sendIssuer bool
	// oauthError denies the attempt with this code instead of a grant;
	// empty grants it.
	oauthError string
	// silent never drives the callback, modeling an abandoned consent
	// page.
	silent bool
}

func (c *consent) open(rawURL string) error {
	go func() {
		if c.delay > 0 {
			time.Sleep(c.delay)
		}
		if c.silent {
			return
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			return
		}
		q := u.Query()
		redirect := q.Get("redirect_uri")
		state := q.Get("state")
		if redirect == "" || state == "" {
			return
		}
		cq := url.Values{}
		cq.Set("state", state)
		if c.oauthError != "" {
			cq.Set("error", c.oauthError)
			cq.Set("error_description", "free text that must not surface")
		} else {
			cq.Set("code", "code-1")
		}
		if c.sendIssuer {
			cq.Set("iss", u.Scheme+"://"+u.Host+"/oauth")
		}
		resp, err := http.Get(redirect + "?" + cq.Encode())
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()
	return nil
}

// recordingNotes captures the reporter's notes and printed URLs. The
// reporter runs on the login goroutine, so access is locked.
type recordingNotes struct {
	mu    sync.Mutex
	notes []string
	urls  []string
}

func (r *recordingNotes) AuthorizationURL(rawURL string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.urls = append(r.urls, rawURL)
}

func (r *recordingNotes) Note(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, fmt.Sprintf(format, args...))
}

func (r *recordingNotes) urlsSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.urls...)
}

// testRuntime assembles one full login runtime on an in-memory credential
// backend and a real SQLite state store, so the durable lease, fence, and
// credential behavior are the production ones.
type testRuntime struct {
	svc     *Service
	client  *oauth.Client
	profile *profile.Profile
	st      *store.Store
	kr      *credential.Keyring
	notes   *recordingNotes
	cleanup func()
}

func newTestRuntime(t *testing.T, fx *oauthFixture, opener func(string) error, deadline time.Duration) *testRuntime {
	t.Helper()
	base := fx.base()
	origin, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	p := &profile.Profile{
		Version:      profile.SchemaVersion,
		Name:         "logintest",
		Origin:       origin.Scheme + "://" + origin.Host,
		Endpoint:     base + "/mcp/app",
		Issuer:       base + "/oauth",
		Instructions: "Fixture instructions.",
		Bounds:       profile.Bounds{ProtocolMin: "2026-07-28", ProtocolMax: "2026-07-28"},
		State:        profile.StateRefs{Database: "default", Credentials: "default"},
		Operations:   []catalog.Descriptor{fixtureOperation()},
		Scopes:       []string{"mcp.message"},
	}
	kr := credential.NewWithBackend("login-test", newMemKeyring())
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.sqlite"), kr, store.Config{Limits: limits.Default()})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	client, err := oauth.New(oauth.Config{
		Endpoint: p.Endpoint,
		Issuer:   p.Issuer,
		Secrets:  kr,
		Lease:    st,
		Scopes:   p.Scopes,
	})
	if err != nil {
		_ = st.Close()
		t.Fatalf("oauth.New: %v", err)
	}
	notes := &recordingNotes{}
	svc, err := New(Options{
		Profile:          p,
		Client:           client,
		Lease:            st,
		OpenBrowser:      opener,
		Reporter:         notes,
		CallbackDeadline: deadline,
	})
	if err != nil {
		_ = st.Close()
		t.Fatalf("login.New: %v", err)
	}
	return &testRuntime{
		svc:     svc,
		client:  client,
		profile: p,
		st:      st,
		kr:      kr,
		notes:   notes,
		cleanup: func() { _ = st.Close() },
	}
}

func fixtureOperation() catalog.Descriptor {
	d := catalog.Descriptor{
		Name:        "message",
		Title:       "Message",
		Description: "Send one message to Tama.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
		TaskSupport: catalog.TaskSupportRequired,
		Strategy:    catalog.StrategyUpstreamTask,
	}
	digest, err := d.ComputeDigest()
	if err != nil {
		panic(err)
	}
	d.Digest = digest
	return d
}

// run drives one login attempt and closes the runtime afterward.
func (r *testRuntime) run(t *testing.T) error {
	t.Helper()
	t.Cleanup(r.cleanup)
	return r.svc.Run(context.Background())
}

// freshClient builds a new OAuth client over the runtime's durable state,
// modeling a second process that finds the committed credential.
func (r *testRuntime) freshClient(t *testing.T) *oauth.Client {
	t.Helper()
	c, err := oauth.New(oauth.Config{
		Endpoint: r.profile.Endpoint,
		Issuer:   r.profile.Issuer,
		Secrets:  r.kr,
		Lease:    r.st,
		Scopes:   r.profile.Scopes,
	})
	if err != nil {
		t.Fatalf("fresh client: %v", err)
	}
	return c
}

func TestLoginSuccessWithBrowser(t *testing.T) {
	t.Parallel()

	fx := (&oauthFixture{tokenScope: "mcp.message"}).start(t)
	c := &consent{delay: 20 * time.Millisecond}
	rt := newTestRuntime(t, fx, c.open, 0)

	if err := rt.run(t); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The browser opened: nothing may have been printed to stdout.
	if urls := rt.notes.urlsSnapshot(); len(urls) != 0 {
		t.Fatalf("stdout urls = %v, want none", urls)
	}
	// The registration and the token exchange both carry the canonical scope.
	if !strings.Contains(fx.regBody, `"scope":"mcp.message"`) {
		t.Fatalf("registration body = %s, want the requested scope", fx.regBody)
	}
	if got := fx.tokenForm.Get("scope"); got != "mcp.message" {
		t.Fatalf("token scope = %q", got)
	}
	if got := fx.tokenForm.Get("resource"); got != rt.profile.Endpoint {
		t.Fatalf("token resource = %q", got)
	}
	ready, err := rt.client.HasCredentials(context.Background())
	if err != nil || !ready {
		t.Fatalf("HasCredentials = %v err=%v, want ready", ready, err)
	}
}

func TestLoginManualHandoff(t *testing.T) {
	t.Parallel()

	fx := (&oauthFixture{}).start(t)
	// A nil opener selects the manual handoff: the URL goes to stdout and
	// the user completes the attempt themselves. The test models that user
	// by driving the callback from the printed URL.
	rt := newTestRuntime(t, fx, nil, 0)
	done := make(chan error, 1)
	go func() { done <- rt.svc.Run(context.Background()) }()
	authURL := waitURL(t, rt.notes)
	t.Cleanup(rt.cleanup)

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse authorization url: %v", err)
	}
	q := u.Query()
	if _, err := http.Get(q.Get("redirect_uri") + "?code=code-1&state=" + q.Get("state")); err != nil {
		t.Fatalf("manual callback: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if urls := rt.notes.urlsSnapshot(); len(urls) != 1 {
		t.Fatalf("stdout urls = %v, want exactly the authorization URL", urls)
	}
	if !strings.HasPrefix(authURL, fx.base()+"/oauth/authorize?") {
		t.Fatalf("authorization url = %q", authURL)
	}
}

// waitURL polls the recorded notes for the manual-handoff URL.
func waitURL(t *testing.T, notes *recordingNotes) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if urls := notes.urlsSnapshot(); len(urls) == 1 {
			return urls[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the manual handoff never printed the authorization URL")
	return ""
}

func TestLoginIssuerRequired(t *testing.T) {
	t.Parallel()

	fx := (&oauthFixture{advertiseIssuer: true}).start(t)

	// With the issuer advertised, a callback that omits iss is rejected
	// and the attempt times out: the fail-closed path.
	rt := newTestRuntime(t, fx, (&consent{sendIssuer: false, delay: 20 * time.Millisecond}).open, 300*time.Millisecond)
	if err := rt.run(t); !errors.Is(err, ErrCallbackTimeout) {
		t.Fatalf("Run = %v, want ErrCallbackTimeout without iss", err)
	}

	// Echoing the validated issuer completes the login.
	rt2 := newTestRuntime(t, fx, (&consent{sendIssuer: true, delay: 20 * time.Millisecond}).open, 0)
	if err := rt2.run(t); err != nil {
		t.Fatalf("Run with iss: %v", err)
	}
}

// TestLoginDeniedPreservesPriorCredential proves a denied re-login leaves
// the still-usable prior credential intact and refreshable.
func TestLoginDeniedPreservesPriorCredential(t *testing.T) {
	t.Parallel()

	fx := (&oauthFixture{}).start(t)

	rt := newTestRuntime(t, fx, (&consent{delay: 20 * time.Millisecond}).open, 0)
	if err := rt.run(t); err != nil {
		t.Fatalf("first login: %v", err)
	}
	// A fresh client over the same durable state proves the credential
	// survives: it must refresh against the fixture.
	if _, err := rt.freshClient(t).Token(context.Background()); err != nil {
		t.Fatalf("prior credential refresh: %v", err)
	}

	// The denied re-login: swap the opener for one that refuses consent.
	denied := &consent{oauthError: "access_denied", delay: 20 * time.Millisecond}
	svc, err := New(Options{
		Profile:     rt.profile,
		Client:      rt.client,
		Lease:       rt.st,
		OpenBrowser: denied.open,
		Reporter:    rt.notes,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = svc.Run(context.Background())
	t.Cleanup(rt.cleanup)
	if err == nil {
		t.Fatal("denied login succeeded")
	}
	if !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("denied login error = %v, want the sanitized code", err)
	}
	if strings.Contains(err.Error(), "free text") {
		t.Fatalf("provider free text leaked: %v", err)
	}

	// The prior credential is still usable.
	if _, err := rt.freshClient(t).Token(context.Background()); err != nil {
		t.Fatalf("prior credential destroyed by a denied re-login: %v", err)
	}
}

func TestLoginCrashRecovery(t *testing.T) {
	// Shared TTL variable: run serially.
	oldTTL := loginLeaseTTL
	loginLeaseTTL = 150 * time.Millisecond
	t.Cleanup(func() { loginLeaseTTL = oldTTL })

	fx := (&oauthFixture{}).start(t)
	rt := newTestRuntime(t, fx, (&consent{delay: 20 * time.Millisecond}).open, 0)

	// A crashed attempt leaves its lease; expiry hands the profile back.
	taken, err := rt.st.ClaimLease(context.Background(), LoginLeaseName, "crashed-process", loginLeaseTTL)
	if err != nil || !taken {
		t.Fatalf("crash claim = %v err=%v", taken, err)
	}
	time.Sleep(250 * time.Millisecond)

	if err := rt.run(t); err != nil {
		t.Fatalf("Run after crash recovery: %v", err)
	}
	ready, err := rt.client.HasCredentials(context.Background())
	if err != nil || !ready {
		t.Fatalf("HasCredentials = %v err=%v", ready, err)
	}
}

func TestLoginLostLeaseAborts(t *testing.T) {
	// Shared TTL variable: run serially.
	oldTTL := loginLeaseTTL
	loginLeaseTTL = 100 * time.Millisecond
	t.Cleanup(func() { loginLeaseTTL = oldTTL })

	fx := (&oauthFixture{}).start(t)
	rt := newTestRuntime(t, fx, (&consent{delay: 20 * time.Millisecond}).open, 0)

	// The losing wrapper reports a lost lease on the first renewal, so
	// the working context cancels mid-attempt.
	svc, err := New(Options{
		Profile:     rt.profile,
		Client:      rt.client,
		Lease:       &leaseLoser{Leaser: rt.st},
		OpenBrowser: (&consent{delay: 60 * time.Millisecond}).open,
		Reporter:    rt.notes,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = svc.Run(context.Background())
	t.Cleanup(rt.cleanup)
	if err == nil {
		t.Fatal("Run succeeded after losing the login lease")
	}
	if ready, err := rt.client.HasCredentials(context.Background()); err == nil && ready {
		t.Fatal("a lease-lost attempt committed a credential")
	}
}

func TestLoginCallbackTimeout(t *testing.T) {
	t.Parallel()

	fx := (&oauthFixture{}).start(t)
	rt := newTestRuntime(t, fx, (&consent{silent: true}).open, 250*time.Millisecond)
	err := rt.svc.Run(context.Background())
	t.Cleanup(rt.cleanup)
	if !errors.Is(err, ErrCallbackTimeout) {
		t.Fatalf("Run = %v, want ErrCallbackTimeout", err)
	}
}

func TestLoginUnadvertisedScopeFailsBeforeRegistration(t *testing.T) {
	t.Parallel()

	fx := (&oauthFixture{prmScopes: `["other.scope"]`}).start(t)
	rt := newTestRuntime(t, fx, (&consent{delay: 20 * time.Millisecond}).open, 0)
	err := rt.svc.Run(context.Background())
	t.Cleanup(rt.cleanup)
	if err == nil {
		t.Fatal("Run accepted a scope set the resource does not advertise")
	}
	fx.mu.Lock()
	regCalls := fx.regCalls
	fx.mu.Unlock()
	if regCalls != 0 {
		t.Fatalf("registration calls = %d, want 0: the scope check runs before registration", regCalls)
	}
}

func TestLoginRegistrationScopeMismatch(t *testing.T) {
	t.Parallel()

	fx := (&oauthFixture{regScope: "other.scope"}).start(t)
	rt := newTestRuntime(t, fx, (&consent{delay: 20 * time.Millisecond}).open, 0)
	if err := rt.run(t); err == nil {
		t.Fatal("Run accepted a registration that cannot grant the requested scope")
	}
}

func TestLoginBusy(t *testing.T) {
	t.Parallel()

	fx := (&oauthFixture{}).start(t)
	rt := newTestRuntime(t, fx, (&consent{delay: 20 * time.Millisecond}).open, 0)

	taken, err := rt.st.ClaimLease(context.Background(), LoginLeaseName, "other-process", 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !taken {
		t.Fatal("test claim failed")
	}
	err = rt.svc.Run(context.Background())
	t.Cleanup(rt.cleanup)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Run = %v, want ErrBusy", err)
	}
}

// leaseLoser wraps a Leaser and reports lost ownership for every login
// lease renewal, so a test can prove a lost login lease aborts the attempt
// before any credential mutation.
type leaseLoser struct {
	Leaser
}

func (l *leaseLoser) RenewLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	if name == LoginLeaseName {
		return false, nil
	}
	return l.Leaser.RenewLease(ctx, name, owner, ttl)
}
