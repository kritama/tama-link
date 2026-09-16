package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/store"
)

// registerServer serves one dynamic client registration endpoint.
func registerServer(t *testing.T, body string, status int, calls *int32) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("register: method = %s", r.Method)
		}
		if atomic.AddInt32(calls, 1) > 0 {
			var body struct {
				ClientName    string   `json:"client_name"`
				RedirectURIs  []string `json:"redirect_uris"`
				GrantTypes    []string `json:"grant_types"`
				ResponseTypes []string `json:"response_types"`
				AuthMethod    string   `json:"token_endpoint_auth_method"`
			}
			raw := make([]byte, 4096)
			n, _ := r.Body.Read(raw)
			if err := json.Unmarshal(raw[:n], &body); err != nil {
				t.Errorf("decode registration body: %v", err)
			}
			if body.ClientName != "Tama Link" {
				t.Errorf("client_name = %q", body.ClientName)
			}
			if len(body.RedirectURIs) != 1 || body.RedirectURIs[0] != "http://127.0.0.1" {
				t.Errorf("redirect_uris = %v", body.RedirectURIs)
			}
			if !contains(body.GrantTypes, "authorization_code") || !contains(body.GrantTypes, "refresh_token") {
				t.Errorf("grant_types = %v", body.GrantTypes)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// regMetadata returns a Metadata whose registration endpoint points at the
// fixture server.
func regMetadata(ts *httptest.Server, issuer string) *Metadata {
	return &Metadata{
		AS: AuthorizationServer{
			Issuer:                   issuer,
			AuthorizationEndpoint:    ts.URL + "/oauth/authorize",
			TokenEndpoint:            ts.URL + "/oauth/token",
			RegistrationEndpoint:     ts.URL + "/register",
			CodeChallengeMethods:     []string{"S256"},
			GrantTypes:               []string{"authorization_code", "refresh_token"},
			TokenEndpointAuthMethods: []string{"client_secret_basic"},
		},
		ASURL: issuer,
	}
}

func TestRegisterDCR(t *testing.T) {
	var calls int32
	ts := registerServer(t, `{"client_id":"cid-1","client_secret":"shh"}`, http.StatusCreated, &calls)
	secrets := newFakeSecrets()
	client := newStaticClient(t, secrets, newFakeLease(), newTestClock(time.Unix(1_700_000_000, 0)))
	md := regMetadata(ts, testIssuer)

	rec, err := client.Register(context.Background(), md)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if rec.ClientID != "cid-1" || rec.ClientSecret != "shh" {
		t.Errorf("record = %+v", rec)
	}
	if rec.Issuer != testIssuer || rec.AuthMethod != "client_secret_basic" {
		t.Errorf("record issuer/auth = %q/%q", rec.Issuer, rec.AuthMethod)
	}
	if stored, found, err := client.RegisteredClient(context.Background()); err != nil || !found || !strings.Contains(stored.ClientID, "cid-1") {
		t.Fatalf("stored = %+v found=%v err=%v", stored, found, err)
	}

	// A second registration reuses the stored record without a new request.
	rec2, err := client.Register(context.Background(), md)
	if err != nil {
		t.Fatalf("reuse Register: %v", err)
	}
	if rec2.ClientID != "cid-1" {
		t.Errorf("reused record = %+v", rec2)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("registration calls = %d, want 1", got)
	}
}

func TestRegisterReuseIssuerMismatch(t *testing.T) {
	var calls int32
	ts := registerServer(t, `{"client_id":"cid-1","client_secret":"shh"}`, http.StatusCreated, &calls)
	secrets := newFakeSecrets()
	client := newStaticClient(t, secrets, newFakeLease(), newTestClock(time.Now()))

	if _, err := client.Register(context.Background(), regMetadata(ts, testIssuer)); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	other := regMetadata(ts, "https://other-issuer.example/oauth")
	if _, err := client.Register(context.Background(), other); err == nil {
		t.Fatal("registration reuse across issuers accepted")
	}
}

// TestRegisteredClientTreatsLegacySecretlessRecordAsAbsent pins the
// upgrade path: a record persisted before the per-method secret check is
// unusable for every exchange, so it must not count as a registered
// client — readiness reports not-ready and the next login re-registers
// instead of the profile looping on the unusable record.
func TestRegisteredClientTreatsLegacySecretlessRecordAsAbsent(t *testing.T) {
	secrets := newFakeSecrets()
	client := newStaticClient(t, secrets, newFakeLease(), newTestClock(time.Unix(1_700_000_000, 0)))

	legacy := ClientRecord{ClientID: "cid-1", AuthMethod: "client_secret_basic", Issuer: testIssuer, RegisteredAt: time.Now().UTC()}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy record: %v", err)
	}
	if err := secrets.SetSecret(labelClient, data); err != nil {
		t.Fatalf("store legacy record: %v", err)
	}

	if _, found, err := client.RegisteredClient(context.Background()); err != nil || found {
		t.Fatalf("RegisteredClient = found:%v err:%v, want absent without an error", found, err)
	}

	// The reuse path must not return it: Register re-registers and the
	// fresh record replaces the legacy one.
	var calls int32
	ts := registerServer(t, `{"client_id":"cid-2","client_secret":"shh"}`, http.StatusCreated, &calls)
	rec, err := client.Register(context.Background(), regMetadata(ts, testIssuer))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if rec.ClientID != "cid-2" || rec.ClientSecret != "shh" {
		t.Fatalf("Register reused or corrupted the record: %+v", rec)
	}
	if got, found, _ := client.RegisteredClient(context.Background()); !found || got.ClientID != "cid-2" {
		t.Fatalf("stored record after re-registration = %q found:%v, want the fresh record", got.ClientID, found)
	}
}

// TestRegisterRollsBackSlotAfterCallerCancellation pins the fenced
// commit under a canceled caller: the slot write itself is
// uninterruptible, so it completes, but the fence commit observes the
// caller context and fails — the uncommitted slot is rolled back and no
// registration is installed, so the retry re-runs cleanly.
func TestRegisterRollsBackSlotAfterCallerCancellation(t *testing.T) {
	secrets := newFakeSecrets()
	secrets.setDelay = 100 * time.Millisecond
	client := newStaticClient(t, secrets, newFakeLease(), newTestClock(time.Unix(1_700_000_000, 0)))

	var calls int32
	ts := registerServer(t, `{"client_id":"cid-1","client_secret":"shh"}`, http.StatusCreated, &calls)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel the caller at the moment the slot write starts.
	secrets.setHook = func(label string) {
		if strings.HasPrefix(label, store.ClientFenceName+"@") {
			cancel()
		}
	}

	if _, err := client.Register(ctx, regMetadata(ts, testIssuer)); err == nil {
		t.Fatal("Register succeeded after the caller canceled mid-commit")
	}
	assertNoClientRecord(context.Background(), t, secrets, client.lease)
}

// TestRegisterRollsBackSlotAfterLostEpochDuringWrite pins the
// lease-bound fence commit: the slot write is uninterruptible, so if a
// foreign process takes over the lease while the write is in flight, the
// commit is rejected and the uncommitted slot is removed again — the
// stale writer can never install a record, and the winner's registration
// is never touched.
func TestRegisterRollsBackSlotAfterLostEpochDuringWrite(t *testing.T) {
	secrets := newFakeSecrets()
	secrets.setDelay = 50 * time.Millisecond
	lease := newFakeLease()
	client := newStaticClient(t, secrets, lease, newTestClock(time.Unix(1_700_000_000, 0)))

	var calls int32
	ts := registerServer(t, `{"client_id":"cid-1","client_secret":"shh"}`, http.StatusCreated, &calls)
	// A foreign process takes over the lease the moment the slot write
	// starts.
	secrets.setHook = func(label string) {
		if strings.HasPrefix(label, store.ClientFenceName+"@") {
			lease.holdOther()
		}
	}

	if _, err := client.Register(context.Background(), regMetadata(ts, testIssuer)); !errors.Is(err, ErrLeaseContention) {
		t.Fatalf("Register = %v, want ErrLeaseContention for the lost epoch", err)
	}
	assertNoClientRecord(context.Background(), t, secrets, client.lease)
}

// assertNoClientRecord pins the absence of any client registration:
// neither the legacy label nor a fenced slot is live, and no uncommitted
// slot leaked into the credential backend.
func assertNoClientRecord(ctx context.Context, t *testing.T, secrets *fakeSecrets, lease Leaser) {
	t.Helper()
	if _, found, err := secrets.GetSecret(labelClient); err != nil || found {
		t.Fatalf("legacy client label = found:%v err:%v, want absent", found, err)
	}
	gen, slot, found, err := lease.ReadCredentialFence(ctx, store.ClientFenceName)
	_ = gen
	if err != nil || found {
		t.Fatalf("client fence = gen:%d slot:%q found:%v err:%v, want absent", gen, slot, found, err)
	}
	for _, label := range secrets.secretLabels() {
		if strings.HasPrefix(label, store.ClientFenceName+"@") {
			t.Fatalf("uncommitted client slot %q leaked into the credential backend", label)
		}
	}
}

// TestRegisterKeepsWinnerRecordAfterLostEpochDuringWrite pins the fence
// isolation: a winning process that took over the lease commits its own
// registration after the stale writer's slot lands. The stale writer's
// commit is rejected and rolls back its own slot, so the winner's record
// survives and its authorization flow can complete.
func TestRegisterKeepsWinnerRecordAfterLostEpochDuringWrite(t *testing.T) {
	secrets := newFakeSecrets()
	secrets.setDelay = 50 * time.Millisecond
	lease := newFakeLease()
	client := newStaticClient(t, secrets, lease, newTestClock(time.Unix(1_700_000_000, 0)))

	var calls int32
	ts := registerServer(t, `{"client_id":"cid-1","client_secret":"shh"}`, http.StatusCreated, &calls)
	// The foreign takeover lands when the slot write starts; the winner's
	// own registration commits immediately after the stale slot is
	// applied.
	winner := &ClientRecord{ClientID: "cid-winner", ClientSecret: "sw", AuthMethod: "client_secret_basic", Issuer: testIssuer, RegisteredAt: time.Now().UTC()}
	winnerData, err := json.Marshal(winner)
	if err != nil {
		t.Fatalf("marshal winner record: %v", err)
	}
	secrets.setHook = func(label string) {
		if strings.HasPrefix(label, store.ClientFenceName+"@") {
			lease.holdOther()
		}
	}
	secrets.setDoneHook = func(label string) {
		if strings.HasPrefix(label, store.ClientFenceName+"@") && label != "oauth-client@winner" {
			if err := secrets.SetSecret("oauth-client@winner", winnerData); err != nil {
				t.Errorf("store winner slot: %v", err)
				return
			}
			// The winner holds the lease: its fence commit succeeds.
			if !lease.fence.commit(store.ClientFenceName, 1, "oauth-client@winner", "other-process") {
				t.Error("the winner's fence commit was rejected")
			}
		}
	}

	if _, err := client.Register(context.Background(), regMetadata(ts, testIssuer)); !errors.Is(err, ErrLeaseContention) {
		t.Fatalf("Register = %v, want ErrLeaseContention for the lost epoch", err)
	}
	// The winner's registration survived the stale writer's rollback.
	stored, found, err := client.RegisteredClient(context.Background())
	if err != nil || !found {
		t.Fatalf("stored record = found:%v err:%v, want the winner's registration", found, err)
	}
	if stored.ClientID != "cid-winner" {
		t.Fatalf("stored record = %+v, want the winner's registration", stored)
	}
}

// TestRegisterRejectsLostLeaseBeforeStoringRecord pins the epoch gate
// on the final write: if the refresh lease is taken over after the claim
// but before the client record is stored, another process has already
// registered and may have committed a matching grant. The stale record
// must not be written over it.
func TestRegisterRejectsLostLeaseBeforeStoringRecord(t *testing.T) {
	secrets := newFakeSecrets()
	lease := newFakeLease()
	// A foreign process takes over the lease immediately after this
	// claim wins.
	lease.onClaim = func() { lease.holdOther() }
	client := newStaticClient(t, secrets, lease, newTestClock(time.Unix(1_700_000_000, 0)))

	var calls int32
	ts := registerServer(t, `{"client_id":"cid-1","client_secret":"shh"}`, http.StatusCreated, &calls)
	if _, err := client.Register(context.Background(), regMetadata(ts, testIssuer)); err == nil {
		t.Fatal("Register succeeded after losing the refresh lease")
	}
	if _, found, _ := secrets.GetSecret(labelClient); found {
		t.Fatal("the stale client record was stored over the winning process's")
	}
}

// TestRegisterSerializesWithCompletion pins the local lock on the
// registration mutation: a same-owner claim succeeds without advancing
// the epoch, so without refreshMu the lease alone cannot order Register
// against this process's own completion, and the second registration
// could store its record before the first client's completion commits
// its grant — pairing the new client with the old one's grant.
func TestRegisterSerializesWithCompletion(t *testing.T) {
	server := (&metadataServer{}).start(t)
	server.tokenDelay = 300 * time.Millisecond
	server.tokenBody = `{"access_token":"at-1","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-1"}`
	secrets := newFakeSecrets()
	lease := newFakeLease()
	client := clientForServer(t, server, secrets, lease, newTestClock(time.Unix(1_700_000_000, 0)))

	// B: a first-time registration whose DCR is slow, started while no
	// record is stored yet, so its reuse check passes and its mutation
	// lands while A is exchanging its code.
	var bCalls int32
	regB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&bCalls, 1)
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"client_id":"cid-b","client_secret":"shb"}`))
	}))
	t.Cleanup(regB.Close)

	regDone := make(chan *ClientRecord, 1)
	go func() {
		fresh, regErr := client.Register(context.Background(), regMetadata(regB, client.issuer))
		if regErr != nil {
			t.Errorf("Register (B): %v", regErr)
			regDone <- nil
			return
		}
		regDone <- fresh
	}()
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&bCalls) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("B's registration never reached the endpoint")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A: the other first-time registration, which wins the record
	// store before B's slow DCR returns.
	var aCalls int32
	regA := registerServer(t, `{"client_id":"cid-a","client_secret":"sha"}`, http.StatusCreated, &aCalls)
	recA, err := client.Register(context.Background(), regMetadata(regA, client.issuer))
	if err != nil {
		t.Fatalf("Register (A): %v", err)
	}
	if recA.ClientID != "cid-a" {
		t.Fatalf("Register (A) = %+v, want cid-a", recA)
	}
	md := serverMetadata(server.ts.URL)
	redirect := "http://127.0.0.1:51234/callback"
	authReq, err := client.NewAuthorizationRequest(md, recA, redirect)
	if err != nil {
		t.Fatalf("NewAuthorizationRequest: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- client.CompleteAuthorization(context.Background(), md, recA, authReq, "code-1", redirect)
	}()
	// Wait until the exchange is in flight at the token endpoint: the
	// completion holds the local lock from here until it commits.
	for {
		server.tokenReqMu.Lock()
		inFlight := server.tokenConcur > 0
		server.tokenReqMu.Unlock()
		if inFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the authorization exchange never reached the token endpoint")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// B's DCR has returned and its mutation section is next: it must
	// wait behind A's completion, so the stored record is still A's
	// while the exchange is in flight.
	time.Sleep(150 * time.Millisecond)
	if stored, found, _ := client.RegisteredClient(context.Background()); !found || stored.ClientID != "cid-a" {
		t.Fatalf("stored record while A exchanges = %+v found:%v, want cid-a: B stored before A's completion finished", stored, found)
	}
	if err := <-done; err != nil {
		t.Fatalf("CompleteAuthorization: %v", err)
	}
	if recB := <-regDone; recB == nil || recB.ClientID != "cid-b" {
		t.Fatalf("Register (B) = %+v, want the fresh record", recB)
	}

	// B observed the committed grant and retired it as orphaned: the
	// new client is not paired with a grant issued under the old one.
	if _, found, _ := secrets.GetSecret(labelRefresh); found {
		t.Fatal("A's grant survived B's re-registration")
	}
	if stored, found, _ := client.RegisteredClient(context.Background()); !found || stored.ClientID != "cid-b" {
		t.Fatalf("stored record = %+v found:%v, want B's registration", stored, found)
	}
}

// TestRegisterRetiresOrphanedCredential pins the repair path: a
// replacement registration issues a new client ID, so a refresh
// credential still stored from the previous client can never refresh
// under it. It is retired before the new record is stored, so readiness
// never pairs the new registration with the orphaned grant.
func TestRegisterRetiresOrphanedCredential(t *testing.T) {
	secrets := newFakeSecrets()
	lease := newFakeLease()
	client := newStaticClient(t, secrets, lease, newTestClock(time.Unix(1_700_000_000, 0)))

	// A legacy secretless record and a refresh credential from the
	// previous client.
	legacy := ClientRecord{ClientID: "cid-old", AuthMethod: "client_secret_basic", Issuer: testIssuer, RegisteredAt: time.Now().UTC()}
	legacyData, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("marshal legacy record: %v", err)
	}
	if err := secrets.SetSecret(labelClient, legacyData); err != nil {
		t.Fatalf("store legacy record: %v", err)
	}
	cred := refreshCredential{RefreshToken: "rt-old", TokenEndpoint: testIssuer + "/token", Issuer: testIssuer, Updated: time.Now().UTC()}
	credData, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("marshal credential: %v", err)
	}
	if err := secrets.SetSecret(labelRefresh, credData); err != nil {
		t.Fatalf("store credential: %v", err)
	}

	var calls int32
	ts := registerServer(t, `{"client_id":"cid-new","client_secret":"shh"}`, http.StatusCreated, &calls)
	rec, err := client.Register(context.Background(), regMetadata(ts, testIssuer))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if rec.ClientID != "cid-new" {
		t.Fatalf("Register = %+v, want the fresh registration", rec)
	}

	// The orphaned grant is gone and readiness does not pair the new
	// client with it.
	if _, found, _ := secrets.GetSecret(labelRefresh); found {
		t.Fatal("the orphaned refresh credential survived the re-registration")
	}
	if ok, err := client.HasCredentials(context.Background()); err != nil || ok {
		t.Fatalf("HasCredentials after re-registration = %v %v, want not ready without an error", ok, err)
	}
}

// TestRegisteredClientHonorsSecretExpiry pins the RFC 7591
// client_secret_expires_at: an expiring registration stops being a usable
// client at its expiry, and the DCR response's field is persisted.
func TestRegisteredClientHonorsSecretExpiry(t *testing.T) {
	secrets := newFakeSecrets()
	clock := newTestClock(time.Unix(1_700_000_000, 0))
	client := newStaticClient(t, secrets, newFakeLease(), clock)

	rec := ClientRecord{
		ClientID:        "cid-1",
		ClientSecret:    "shh",
		AuthMethod:      "client_secret_basic",
		Issuer:          testIssuer,
		RegisteredAt:    time.Unix(1_700_000_000, 0).UTC(),
		SecretExpiresAt: 1_700_000_000 + 3600,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if err := secrets.SetSecret(labelClient, data); err != nil {
		t.Fatalf("store record: %v", err)
	}

	if _, found, err := client.RegisteredClient(context.Background()); err != nil || !found {
		t.Fatalf("unexpired RegisteredClient = found:%v err:%v, want present", found, err)
	}

	clock.set(time.Unix(1_700_000_000+3601, 0))
	if _, found, err := client.RegisteredClient(context.Background()); err != nil || found {
		t.Fatalf("expired RegisteredClient = found:%v err:%v, want absent without an error", found, err)
	}
	if ok, err := client.HasCredentials(context.Background()); err != nil || ok {
		t.Fatalf("HasCredentials with an expired secret = %v %v, want not ready", ok, err)
	}

	// The DCR response's expiration is decoded and persisted.
	var calls int32
	ts := registerServer(t, `{"client_id":"cid-2","client_secret":"shh","client_secret_expires_at":1800000000}`, http.StatusCreated, &calls)
	fresh, err := client.Register(context.Background(), regMetadata(ts, testIssuer))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if fresh.SecretExpiresAt != 1800000000 {
		t.Fatalf("persisted client_secret_expires_at = %d, want 1800000000", fresh.SecretExpiresAt)
	}
}

func TestRegisterFailures(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"non-2xx", `{"error":"server_error"}`, http.StatusInternalServerError},
		{"missing client id", `{}`, http.StatusCreated},
		{"missing client secret", `{"client_id":"cid-1"}`, http.StatusCreated},
		{"malformed", `{not json`, http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls int32
			ts := registerServer(t, tc.body, tc.status, &calls)
			client := newStaticClient(t, newFakeSecrets(), newFakeLease(), newTestClock(time.Now()))
			if _, err := client.Register(context.Background(), regMetadata(ts, testIssuer)); err == nil {
				t.Fatal("registration accepted")
			}
		})
	}
}
