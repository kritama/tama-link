package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/store"
)

// fakeSecrets is an in-memory SecretStore for tests. setDelay blocks
// SetSecret so a test can hold the credential write open past a lease TTL;
// deleteDelay blocks DeleteSecret the same way for the cleanup path.
// failDelete makes DeleteSecret fail for one label so a test can prove a
// deletion failure stays recoverable.
type fakeSecrets struct {
	mu          sync.Mutex
	items       map[string][]byte
	setDelay    time.Duration
	deleteDelay time.Duration
	failDelete  map[string]error
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{items: map[string][]byte{}}
}

// secretLabels lists the stored labels, for slot-leak assertions.
func (f *fakeSecrets) secretLabels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.items))
	for label := range f.items {
		out = append(out, label)
	}
	return out
}

// delayedSecrets fronts a shared fake store with one writer's own set delay,
// so two fake processes can share the durable store while one write blocks.
type delayedSecrets struct {
	inner *fakeSecrets
	delay time.Duration
}

func (d *delayedSecrets) GetSecret(label string) ([]byte, bool, error) {
	return d.inner.GetSecret(label)
}

func (d *delayedSecrets) SetSecret(label string, data []byte) error {
	if d.delay > 0 {
		time.Sleep(d.delay)
	}
	d.inner.mu.Lock()
	defer d.inner.mu.Unlock()
	out := make([]byte, len(data))
	copy(out, data)
	d.inner.items[label] = out
	return nil
}

func (d *delayedSecrets) DeleteSecret(label string) error {
	return d.inner.DeleteSecret(label)
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
	if f.setDelay > 0 {
		time.Sleep(f.setDelay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]byte, len(data))
	copy(out, data)
	f.items[label] = out
	return nil
}

func (f *fakeSecrets) DeleteSecret(label string) error {
	f.mu.Lock()
	if f.deleteDelay > 0 {
		d := f.deleteDelay
		f.mu.Unlock()
		time.Sleep(d)
		f.mu.Lock()
	}
	defer f.mu.Unlock()
	if err, ok := f.failDelete[label]; ok {
		return err
	}
	delete(f.items, label)
	return nil
}

// failDeletes makes the next DeleteSecret calls fail for the labels until
// allowDeletes clears them.
func (f *fakeSecrets) failDeletes(labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete == nil {
		f.failDelete = map[string]error{}
	}
	for _, label := range labels {
		f.failDelete[label] = errors.New("backend write failure")
	}
}

// allowDeletes clears the deletion failures for the labels.
func (f *fakeSecrets) allowDeletes(labels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, label := range labels {
		delete(f.failDelete, label)
	}
}

// recordingSecrets counts SetSecret calls so a test can prove a gated
// refresh never reached the credential write.
type recordingSecrets struct {
	*fakeSecrets
	sets int
}

func (r *recordingSecrets) SetSecret(label string, data []byte) error {
	r.sets++
	return r.fakeSecrets.SetSecret(label, data)
}

// fakeLease is an in-memory Leaser. holdOther simulates another process
// holding the lease; onClaim runs just before a claim succeeds, letting a
// test write a replacement credential while the claimant waits.
type fakeLease struct {
	mu         sync.Mutex
	holder     string
	onClaim    func()
	claims     int
	released   int
	renewals   int
	loseRenew  bool
	loseAfter  int
	generation int

	fence          *sharedFence
	fenceCommitErr error
	// retired records the retirement backlog: slots whose deletion failed
	// and that a later refresh or logout must retry.
	retired map[string]struct{}
}

func newFakeLease() *fakeLease {
	f := &fakeLease{}
	// The shared state always exists: in production the lease table and
	// the credential fence live in the same SQLite store. Two simulated
	// processes share it by pointing one lease's fence at the other's.
	f.fence = newSharedFence()
	return f
}

// sharedFence models the durable state two simulated processes share: the
// credential fence, and the refresh-lease holder the fence commit is bound
// to. Each process keeps its own fakeLease claim view, but a claim by
// either process replaces the shared holder, which is what a lease-bound
// fence commit checks. The fake models ownership only; the generation
// arithmetic is covered by the store's own tests.
type sharedFence struct {
	mu         sync.Mutex
	generation int64
	slot       string
	found      bool
	// lease ownership the fence commit verifies.
	leaseHolder string
}

func newSharedFence() *sharedFence { return &sharedFence{} }

func (f *sharedFence) read() (int64, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generation, f.slot, f.found
}

// claimLease records one claim on the shared lease table: the last
// claimer owns the lease, matching the test scenarios where a foreign
// process takes over after the first holder's lease lapsed.
func (f *sharedFence) claimLease(owner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaseHolder = owner
}

func (f *sharedFence) releaseLease(owner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leaseHolder == owner {
		f.leaseHolder = ""
	}
}

func (f *sharedFence) commit(generation int64, slot, leaseOwner string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leaseHolder != leaseOwner {
		return false
	}
	if generation <= f.generation {
		return false
	}
	f.generation = generation
	f.slot = slot
	f.found = true
	return true
}

// clearIfOwned clears the fence only when leaseOwner still owns the
// shared lease. Returns whether it cleared.
func (f *sharedFence) clearIfOwned(leaseOwner string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.leaseHolder != leaseOwner {
		return false
	}
	f.generation = 0
	f.slot = ""
	f.found = false
	return true
}

func (f *fakeLease) holdOther() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holder = "other-process"
	if f.fence != nil {
		f.fence.claimLease("other-process")
	}
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
	f.holder = owner
	if f.fence != nil {
		f.fence.claimLease(owner)
	}
	hook := f.onClaim
	f.mu.Unlock()
	if hook != nil {
		// Runs with the claim recorded, so a hook can model a foreign
		// takeover immediately after this claim wins.
		hook()
	}
	return true, nil
}

func (f *fakeLease) LeaseGeneration(_ context.Context, name, owner string) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != refreshLeaseName {
		return 0, false, fmt.Errorf("unexpected lease name %q", name)
	}
	if f.holder == owner {
		return int64(f.generation), true, nil
	}
	return 0, false, nil
}

func (f *fakeLease) CommitLease(_ context.Context, name, owner string, generation int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != refreshLeaseName {
		return false, fmt.Errorf("unexpected lease name %q", name)
	}
	return f.holder == owner && int64(f.generation) == generation, nil
}

func (f *fakeLease) ReadCredentialFence(_ context.Context) (int64, string, bool, error) {
	if f.fence == nil {
		return 0, "", false, nil
	}
	gen, slot, found := f.fence.read()
	return gen, slot, found, nil
}

// CommitCredentialFence models the production gate: the fence generation
// compare-and-swap plus the shared lease holder the commit must still own.
// The fake checks ownership only; the epoch arithmetic is covered by the
// store's own tests.
func (f *fakeLease) CommitCredentialFence(_ context.Context, commit store.CredentialFenceCommit) (bool, error) {
	if commit.LeaseName != refreshLeaseName {
		return false, fmt.Errorf("unexpected lease name %q", commit.LeaseName)
	}
	if f.fenceCommitErr != nil {
		return false, f.fenceCommitErr
	}
	if !f.fence.commit(commit.FenceGeneration, commit.Slot, commit.LeaseOwner) {
		return false, nil
	}
	// The previous slot is atomically enqueued for retirement retry with
	// the advance, mirroring the production transaction.
	if commit.PreviousSlot != "" {
		f.retire(commit.PreviousSlot)
	}
	return true, nil
}

func (f *fakeLease) ClearCredentialFence(_ context.Context, leaseName, leaseOwner string, leaseGeneration int64) (bool, error) {
	if leaseName != refreshLeaseName {
		return false, fmt.Errorf("unexpected lease name %q", leaseName)
	}
	if f.fence == nil {
		// An absent fence is successfully cleared.
		return true, nil
	}
	if f.leaseGen() != leaseGeneration {
		return false, nil
	}
	return f.fence.clearIfOwned(leaseOwner), nil
}

// leaseGen reports the caller's captured ownership epoch. The fake's
// per-process generation is always zero, matching the captured value.
func (f *fakeLease) leaseGen() int64 { return 0 }

func (f *fakeLease) retire(slot string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.retired == nil {
		f.retired = map[string]struct{}{}
	}
	f.retired[slot] = struct{}{}
}

func (f *fakeLease) RetiredCredentialSlots(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	slots := make([]string, 0, len(f.retired))
	for slot := range f.retired {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	return slots, nil
}

func (f *fakeLease) ClearRetiredCredentialSlot(_ context.Context, slot string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.retired, slot)
	return nil
}

func (f *fakeLease) RecordRetiredCredentialSlot(_ context.Context, slot string) error {
	f.retire(slot)
	return nil
}

// renewLease makes the next renewal report lost ownership, so the test can
// prove a refresh aborts when it loses the lease mid-exchange.
func (f *fakeLease) loseOnRenew() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loseRenew = true
}

func (f *fakeLease) RenewLease(_ context.Context, name, owner string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != refreshLeaseName || ttl <= 0 {
		return false, fmt.Errorf("unexpected lease name %q ttl %s", name, ttl)
	}
	f.renewals++
	if f.loseRenew {
		f.holder = ""
		if f.fence != nil {
			f.fence.releaseLease(owner)
		}
		return false, nil
	}
	if f.loseAfter > 0 && f.renewals >= f.loseAfter {
		f.holder = ""
		if f.fence != nil {
			f.fence.releaseLease(owner)
		}
		return false, nil
	}
	if f.holder == owner {
		return true, nil
	}
	return false, nil
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
		if f.fence != nil {
			f.fence.releaseLease(owner)
		}
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
	t              *testing.T
	ts             *httptest.Server
	prm            string
	as             string
	tokenBody      string
	tokenDelay     time.Duration
	tokenReq       *http.Request
	tokenRaw       []byte
	tokenCalls     int
	tokenConcur    int
	tokenMaxConcur int
	tokenReqMu     sync.Mutex
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
			// Track the maximum concurrent in-flight token requests so a
			// test can prove one process never rotates a refresh grant
			// twice at once.
			s.tokenReqMu.Lock()
			s.tokenConcur++
			if s.tokenConcur > s.tokenMaxConcur {
				s.tokenMaxConcur = s.tokenConcur
			}
			s.tokenReqMu.Unlock()
			defer func() {
				s.tokenReqMu.Lock()
				s.tokenConcur--
				s.tokenReqMu.Unlock()
			}()
			if s.tokenDelay > 0 {
				select {
				case <-time.After(s.tokenDelay):
				case <-r.Context().Done():
					return
				}
			}
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
func clientForServer(t *testing.T, server *metadataServer, secrets SecretStore, lease *fakeLease, clock *testClock) *Client {
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

// liveCredential reads the credential the fence points at (or the legacy
// label when no fence has been committed), for test assertions.
func liveCredential(t *testing.T, lease *fakeLease, secrets *fakeSecrets) string {
	t.Helper()
	_, slot, found, err := lease.ReadCredentialFence(context.Background())
	if err != nil {
		t.Fatalf("ReadCredentialFence: %v", err)
	}
	label := labelRefresh
	if found {
		label = slot
	}
	data, ok, err := secrets.GetSecret(label)
	if err != nil || !ok {
		t.Fatalf("credential at %s: found=%v err=%v", label, ok, err)
	}
	return string(data)
}
