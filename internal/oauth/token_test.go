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
	data := liveCredential(t, lease, secrets)
	if !strings.Contains(data, "rt-1") {
		t.Errorf("stable refresh token lost: %s", data)
	}
	if lease.released != 2 {
		t.Errorf("lease releases = %d, want 2", lease.released)
	}
}

func TestTokenReplacementRefresh(t *testing.T) {
	server, client, secrets, lease, _ := tokenFixture(t, "rt-1")
	server.tokenBody = `{"access_token":"at-2","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2"}`

	if _, err := client.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	data := liveCredential(t, lease, secrets)
	if !strings.Contains(data, "rt-2") || strings.Contains(data, "rt-1") {
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

// TestRefreshInvalidGrantInvalidatesDurableCredential pins that a rejected
// grant invalidates the durable refresh credential, not just the in-memory
// token: readiness must report the profile as unauthenticated so new work
// fails as authentication_required instead of burning idempotency keys on
// the same known-invalid grant.
func TestRefreshInvalidGrantInvalidatesDurableCredential(t *testing.T) {
	server, client, secrets, lease, clock := tokenFixture(t, "rt-1")
	ctx := context.Background()
	if _, err := client.Token(ctx); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	// The fenced credential is live and readiness is true.
	if ok, err := client.HasCredentials(ctx); err != nil || !ok {
		t.Fatalf("HasCredentials before rejection = %v %v, want true", ok, err)
	}

	// Expire the token; the authorization server rejects the grant.
	clock.set(clock.now.Add(2 * time.Hour))
	server.tokenBody = `{"error":"invalid_grant"}`
	if _, err := client.Token(ctx); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("err = %v, want ErrGrantInvalid", err)
	}
	// The durable credential is gone: the fence is cleared, the slot and
	// the legacy label are deleted, and readiness reports false so submit
	// rejects new work instead of accepting doomed submissions.
	if _, _, found, err := lease.ReadCredentialFence(ctx); err != nil || found {
		t.Fatalf("fence after invalid grant = found:%v err:%v, want cleared", found, err)
	}
	if ok, err := client.HasCredentials(ctx); err != nil || ok {
		t.Fatalf("HasCredentials after invalid grant = %v %v, want false", ok, err)
	}
	if _, found, _ := secrets.GetSecret(labelRefresh); found {
		t.Error("legacy refresh credential survived invalid_grant")
	}
	// The client registration survives: reauthorization reuses it.
	if _, found, _ := secrets.GetSecret(labelClient); !found {
		t.Error("client registration must survive invalid_grant for reauthorization")
	}
	clock.set(clock.now.Add(time.Minute))
}

func TestRefreshLeaseContention(t *testing.T) {
	_, client, _, lease, _ := tokenFixture(t, "rt-1")
	lease.holdOther()

	start := time.Now()
	_, err := client.Token(context.Background())
	if !errors.Is(err, ErrLeaseContention) {
		t.Fatalf("err = %v, want ErrLeaseContention", err)
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
	ctx := context.Background()
	if ok, err := client.HasCredentials(ctx); err != nil || !ok {
		t.Fatalf("HasCredentials = %v %v", ok, err)
	}
	if _, err := client.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if err := client.Logout(ctx); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	// The fence is cleared with the slots: the probe must report no
	// credentials — not follow the fence to a deleted slot and fail the
	// backend — so a logged-out profile can log in again.
	if ok, err := client.HasCredentials(ctx); err != nil || ok {
		t.Fatalf("HasCredentials after logout = %v %v, want false without a backend error", ok, err)
	}
	if _, err := client.Token(context.Background()); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("Token after logout = %v, want ErrNoCredentials", err)
	}
	if _, ok := client.Expiry(); ok {
		t.Error("in-memory token survived logout")
	}
	for _, label := range secrets.secretLabels() {
		if label != labelClient && label != labelRefresh && !fencedSlot(label) {
			continue
		}
		if _, found, _ := secrets.GetSecret(label); found {
			t.Errorf("credential %q survived logout", label)
		}
	}
}

// fencedSlot reports whether label looks like a per-transaction fenced
// credential slot (labelRefresh@hex).
func fencedSlot(label string) bool {
	return strings.HasPrefix(label, labelRefresh+"@")
}

// TestRefreshRetiresPreviousSlot pins that each successful rotation leaves
// exactly one live credential slot: the previous live slot and the legacy
// label are deleted after the fence commit, so historical grants — still
// valid when the provider omits a replacement refresh token — cannot
// survive logout.
func TestRefreshRetiresPreviousSlot(t *testing.T) {
	server, client, secrets, _, clock := tokenFixture(t, "rt-1")
	ctx := context.Background()

	if _, err := client.Token(ctx); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	_, slot1, found, err := client.lease.ReadCredentialFence(ctx)
	if err != nil || !found {
		t.Fatalf("fence after first refresh: %v", err)
	}

	// Expire and rotate again: the provider omits a replacement refresh
	// token, so the second slot holds the same still-valid grant.
	clock.set(clock.now.Add(2 * time.Hour))
	server.tokenBody = `{"access_token":"at-2","token_type":"Bearer","expires_in":3600}`
	if _, err := client.Token(ctx); err != nil {
		t.Fatalf("second Token: %v", err)
	}
	_, slot2, found, err := client.lease.ReadCredentialFence(ctx)
	if err != nil || !found || slot2 == slot1 {
		t.Fatalf("fence after second refresh: slot=%q prev=%q found=%v err=%v", slot2, slot1, found, err)
	}
	if _, ok, _ := secrets.GetSecret(slot1); ok {
		t.Errorf("previous live slot %q survived the rotation", slot1)
	}
	for _, label := range secrets.secretLabels() {
		if label == labelClient || label == slot2 {
			continue
		}
		if _, ok, _ := secrets.GetSecret(label); ok {
			t.Errorf("retired credential entry %q survived", label)
		}
	}
	clock.set(clock.now.Add(time.Minute))
}

// TestHasCredentialsSeesFencedCredential pins the fence-aware probe: after
// any successful exchange the refresh credential lives only at the committed
// fence slot (the legacy label is retired), and the probe must still report
// the profile as ready.
func TestHasCredentialsSeesFencedCredentialOnly(t *testing.T) {
	_, client, secrets, _, _ := tokenFixture(t, "rt-1")
	ctx := context.Background()
	if _, err := client.Token(ctx); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if _, found, _ := secrets.GetSecret(labelRefresh); found {
		t.Fatal("legacy refresh label survived the fenced store")
	}
	if ok, err := client.HasCredentials(ctx); err != nil || !ok {
		t.Fatalf("HasCredentials behind the fence = %v %v, want true", ok, err)
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

// TestLogoutInvalidatesConcurrentRefresh proves a logout that overlaps a
// credential write in another process leaves nothing behind: logout claims
// the refresh lease first, so the in-flight writer's fence commit fails
// under the advanced epoch and its own slot is rolled back — no usable
// credential survives a successful logout.
func TestLogoutInvalidatesConcurrentRefresh(t *testing.T) {
	server := (&metadataServer{}).start(t)
	server.tokenBody = `{"access_token":"at-2","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2"}`
	server.tokenDelay = 80 * time.Millisecond
	shared := newFakeSecrets()
	clock := newTestClock(time.Unix(1_700_000_000, 0))

	leaseA := newFakeLease()
	leaseB := newFakeLease()
	leaseB.fence = leaseA.fence
	clientA := clientForServer(t, server, &delayedSecrets{inner: shared, delay: 150 * time.Millisecond}, leaseA, clock)
	clientB := clientForServer(t, server, shared, leaseB, clock)
	seedCredentials(t, shared, "cid-1", "shh", server.ts.URL+"/oauth/token", server.ts.URL+"/oauth", "rt-1")

	refreshed := make(chan error, 1)
	go func() {
		_, err := clientA.Token(context.Background())
		refreshed <- err
	}()
	// Logout lands while A's exchange and write are in flight: the claim
	// advances the epoch, A's commit is rejected, and A rolls back its own
	// orphan slot.
	time.Sleep(120 * time.Millisecond)
	if err := clientB.Logout(context.Background()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if err := <-refreshed; err == nil {
		t.Fatal("the overlapping refresh succeeded behind the logout")
	}
	// No credential survives: every slot is rolled back or deleted, the
	// fence is cleared, and the probe reports no credentials.
	for _, label := range shared.secretLabels() {
		if _, ok, _ := shared.GetSecret(label); ok {
			t.Errorf("credential %q survived the logout", label)
		}
	}
	if ok, err := clientB.HasCredentials(context.Background()); err != nil || ok {
		t.Fatalf("HasCredentials after logout = %v %v, want false", ok, err)
	}
	clock.set(clock.now.Add(time.Minute))
}

// TestLogoutContendedFailsRetryable pins that logout never proceeds while
// another process holds the refresh lease: it fails with contention
// without touching any credential, so a retried logout completes the
// cleanup.
func TestLogoutContendedFailsRetryable(t *testing.T) {
	_, client, secrets, lease, _ := tokenFixture(t, "rt-1")
	lease.holdOther()
	if err := client.Logout(context.Background()); !errors.Is(err, ErrLeaseContention) {
		t.Fatalf("Logout = %v, want ErrLeaseContention", err)
	}
	if _, found, _ := secrets.GetSecret(labelRefresh); !found {
		t.Fatal("a contended logout deleted credentials")
	}
}

// TestLogoutRetriesAfterBackendFailure pins the recoverable cleanup order:
// the committed slot is deleted while the fence still references it, so a
// failed deletion leaves the slot discoverable and a retried logout
// finishes the cleanup instead of stranding the credential.
func TestLogoutRetriesAfterBackendFailure(t *testing.T) {
	_, client, secrets, lease, _ := tokenFixture(t, "rt-1")
	ctx := context.Background()
	if _, err := client.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	_, slot, found, err := lease.ReadCredentialFence(ctx)
	if err != nil || !found {
		t.Fatalf("fence after refresh: %v", err)
	}
	secrets.failDeletes(slot)
	if err := client.Logout(ctx); err == nil {
		t.Fatal("Logout succeeded despite the backend failure")
	}
	// The fence still references the slot, so the credential is not
	// stranded: the retried logout discovers and deletes it.
	if _, ok, _ := secrets.GetSecret(slot); !ok {
		t.Fatal("the failed deletion lost the slot; the credential is stranded")
	}
	secrets.allowDeletes(slot)
	if err := client.Logout(ctx); err != nil {
		t.Fatalf("retried Logout: %v", err)
	}
	if _, ok, _ := secrets.GetSecret(slot); ok {
		t.Fatal("the retried logout left the credential behind")
	}
	if ok, err := client.HasCredentials(ctx); err != nil || ok {
		t.Fatalf("HasCredentials after retried logout = %v %v", ok, err)
	}
}

// TestCompleteAuthorizationContentionDoesNotBurnCode pins the claim order:
// the refresh lease is claimed before the single-use authorization code is
// redeemed, so a lease contention fails the login before any token request
// and a retried callback can still complete.
func TestCompleteAuthorizationContentionDoesNotBurnCode(t *testing.T) {
	server := (&metadataServer{}).start(t)
	server.tokenBody = `{"access_token":"at-1","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-1"}`
	secrets := newFakeSecrets()
	clock := newTestClock(time.Unix(1_700_000_000, 0))
	lease := newFakeLease()
	client := clientForServer(t, server, secrets, lease, clock)
	rec := &ClientRecord{ClientID: "cid-1", ClientSecret: "shh", AuthMethod: "client_secret_basic", Issuer: client.issuer}
	md := serverMetadata(server.ts.URL)
	observed := "http://127.0.0.1:51234/callback"
	authReq, err := client.NewAuthorizationRequest(md, rec, observed)
	if err != nil {
		t.Fatalf("NewAuthorizationRequest: %v", err)
	}

	lease.holdOther()
	if err := client.CompleteAuthorization(context.Background(), md, rec, authReq, "code-1", observed); !errors.Is(err, ErrLeaseContention) {
		t.Fatalf("CompleteAuthorization = %v, want ErrLeaseContention", err)
	}
	server.tokenReqMu.Lock()
	calls := server.tokenCalls
	server.tokenReqMu.Unlock()
	if calls != 0 {
		t.Fatalf("token endpoint consumed the single-use code %d times under contention", calls)
	}
	clock.set(clock.now.Add(time.Minute))
}

// TestRefreshRetiresPreviousSlotAfterBackendFailure pins the durable
// retirement backlog: a previous-slot deletion that fails is recorded
// durably and the refresh still succeeds — the committed slot is the only
// live credential — and a later refresh drains the backlog, deleting the
// old slot and clearing its record instead of silently stranding a
// still-valid grant.
func TestRefreshRetiresPreviousSlotAfterBackendFailure(t *testing.T) {
	server, client, secrets, lease, clock := tokenFixture(t, "rt-1")
	ctx := context.Background()
	if _, err := client.Token(ctx); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	_, slot1, found, err := lease.ReadCredentialFence(ctx)
	if err != nil || !found {
		t.Fatalf("fence after first refresh: %v", err)
	}

	clock.set(clock.now.Add(2 * time.Hour))
	server.tokenBody = `{"access_token":"at-2","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2"}`
	secrets.failDeletes(slot1)
	if _, err := client.Token(ctx); err != nil {
		t.Fatalf("second Token: %v", err)
	}
	// The new credential is live and the refresh succeeded; the old slot
	// is no longer referenced but still present, and its deletion is
	// durably recorded for retry.
	_, slot2, found, err := lease.ReadCredentialFence(ctx)
	if err != nil || !found || slot2 == slot1 {
		t.Fatalf("fence after second refresh: %q found:%v err:%v", slot2, found, err)
	}
	if _, ok, _ := secrets.GetSecret(slot1); !ok {
		t.Fatal("the old slot vanished without its deletion succeeding")
	}
	pending, err := lease.RetiredCredentialSlots(ctx)
	if err != nil || len(pending) != 1 || pending[0] != slot1 {
		t.Fatalf("retirement backlog = %v err=%v, want [slot1]", pending, err)
	}

	// With the backend healthy again, the next refresh drains the backlog
	// and retires the old slot.
	secrets.allowDeletes(slot1)
	clock.set(clock.now.Add(2 * time.Hour))
	if _, err := client.Token(ctx); err != nil {
		t.Fatalf("third Token: %v", err)
	}
	if _, ok, _ := secrets.GetSecret(slot1); ok {
		t.Fatal("the drained refresh left the previous slot behind")
	}
	pending, err = lease.RetiredCredentialSlots(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("retirement backlog after drain = %v err=%v, want empty", pending, err)
	}
	clock.set(clock.now.Add(time.Minute))
}

// TestRefreshRenewsLeaseThroughPersistence pins that renewal spans the
// credential persistence, not just the token exchange: a slow secret-store
// write that outlives the original lease TTL must not reject the
// replacement after the endpoint already rotated the grant.
func TestRefreshRenewsLeaseThroughPersistence(t *testing.T) {
	server := (&metadataServer{}).start(t)
	server.tokenBody = `{"access_token":"at-2","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2"}`
	shared := newFakeSecrets()
	clock := newTestClock(time.Unix(1_700_000_000, 0))
	lease := newFakeLease()
	client := clientForServer(t, server, &delayedSecrets{inner: shared, delay: 400 * time.Millisecond}, lease, clock)
	seedCredentials(t, shared, "cid-1", "shh", server.ts.URL+"/oauth/token", client.issuer, "rt-1")

	prev := refreshLeaseTTL
	refreshLeaseTTL = 100 * time.Millisecond
	defer func() { refreshLeaseTTL = prev }()

	if _, err := client.Token(context.Background()); err != nil {
		t.Fatalf("Token with a write outliving the lease TTL: %v", err)
	}
	// The replacement is committed: the fence points at the new slot.
	_, slot, found, err := lease.ReadCredentialFence(context.Background())
	if err != nil || !found {
		t.Fatalf("fence after slow write: %v", err)
	}
	data, ok, serr := shared.GetSecret(slot)
	if serr != nil || !ok || !strings.Contains(string(data), "rt-2") {
		t.Fatalf("live credential = %s ok:%v err:%v", data, ok, serr)
	}
	clock.set(clock.now.Add(time.Minute))
}

// TestLogoutLostLeaseKeepsRetryableState pins the epoch-bound cleanup: a
// logout whose lease is lost mid-cleanup fails retryably and does not
// clear a fence it no longer owns — the slot stays discoverable so a
// retried logout finishes the cleanup.
func TestLogoutLostLeaseKeepsRetryableState(t *testing.T) {
	_, client, secrets, lease, clock := tokenFixture(t, "rt-1")
	ctx := context.Background()
	if _, err := client.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	_, _, found, err := lease.ReadCredentialFence(ctx)
	if err != nil || !found {
		t.Fatalf("fence after refresh: %v", err)
	}

	// The first renewal fails mid-cleanup while the slow slot deletion is
	// still running: the epoch-bound fence clear must refuse.
	lease.loseOnRenew()
	shared := secrets
	shared.deleteDelay = 60 * time.Millisecond
	prev := refreshLeaseTTL
	refreshLeaseTTL = 30 * time.Millisecond
	defer func() {
		refreshLeaseTTL = prev
		shared.deleteDelay = 0
	}()
	if err := client.Logout(ctx); err == nil {
		t.Fatal("Logout succeeded after losing its lease mid-cleanup")
	}
	// The fence was not cleared by a lease it no longer owned: the slot
	// record keeps the cleanup retryable.
	if _, _, stillFound, ferr := lease.ReadCredentialFence(ctx); ferr != nil || !stillFound {
		t.Fatalf("fence after lost-lease logout: found=%v err=%v, want still referenced", stillFound, ferr)
	}
	clock.set(clock.now.Add(time.Minute))
}

// TestRefreshInvalidGrantRecordsUndeletableSlot pins the cleanup ordering
// and durability of an invalid_grant invalidation: the fence pointer is
// cleared before the slot is deleted, and a slot whose deletion fails is
// recorded durably for cleanup retry — the durable state is never left
// pointing at a missing slot.
func TestRefreshInvalidGrantRecordsUndeletableSlot(t *testing.T) {
	server, client, secrets, lease, clock := tokenFixture(t, "rt-1")
	ctx := context.Background()
	if _, err := client.Token(ctx); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	_, slot, found, err := lease.ReadCredentialFence(ctx)
	if err != nil || !found {
		t.Fatalf("fence after refresh: %v", err)
	}

	// Expire the token; the authorization server rejects the grant and the
	// slot deletion fails.
	clock.set(clock.now.Add(2 * time.Hour))
	server.tokenBody = `{"error":"invalid_grant"}`
	secrets.failDeletes(slot)
	if _, err := client.Token(ctx); !errors.Is(err, ErrGrantInvalid) {
		t.Fatalf("err = %v, want ErrGrantInvalid", err)
	}
	// The pointer is cleared, so readiness fails closed instead of
	// reporting a dangling fence; the undeletable slot is durably recorded
	// for cleanup.
	if _, _, stillFound, ferr := lease.ReadCredentialFence(ctx); ferr != nil || stillFound {
		t.Fatalf("fence after invalid grant = found:%v err:%v, want cleared", stillFound, ferr)
	}
	if ok, err := client.HasCredentials(ctx); err != nil || ok {
		t.Fatalf("HasCredentials after invalid grant = %v %v, want false without a backend error", ok, err)
	}
	pending, err := lease.RetiredCredentialSlots(ctx)
	if err != nil || len(pending) != 1 || pending[0] != slot {
		t.Fatalf("retirement backlog = %v err=%v, want [slot]", pending, err)
	}

	// The next logout drains the backlog and removes the blob.
	secrets.allowDeletes(slot)
	if err := client.Logout(ctx); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, ok, _ := secrets.GetSecret(slot); ok {
		t.Fatal("the undrainable slot survived logout")
	}
	pending, err = lease.RetiredCredentialSlots(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("retirement backlog after logout = %v err=%v, want empty", pending, err)
	}
	clock.set(clock.now.Add(time.Minute))
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

// TestRefreshRenewsLeaseDuringSlowExchange pins the lease contract: the
// refresh lease is renewed while the token exchange runs, so an exchange
// that outlasts the TTL keeps its ownership instead of leaking it.
func TestRefreshRenewsLeaseDuringSlowExchange(t *testing.T) {
	server, client, _, lease, clock := tokenFixture(t, "rt-1")
	server.tokenDelay = 400 * time.Millisecond
	prev := refreshLeaseTTL
	refreshLeaseTTL = 100 * time.Millisecond
	defer func() { refreshLeaseTTL = prev }()

	if _, err := client.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if lease.renewals == 0 {
		t.Fatal("the slow exchange never renewed the refresh lease")
	}
	clock.set(clock.now.Add(time.Minute))
}

// TestRefreshAbortsWhenLeaseLost pins the loss path: a renewal that loses
// ownership cancels the exchange before any replacement credential is
// persisted.
func TestRefreshAbortsWhenLeaseLost(t *testing.T) {
	server, client, secrets, lease, _ := tokenFixture(t, "rt-1")
	server.tokenDelay = 400 * time.Millisecond
	prev := refreshLeaseTTL
	refreshLeaseTTL = 100 * time.Millisecond
	defer func() { refreshLeaseTTL = prev }()
	lease.loseOnRenew()

	start := time.Now()
	_, err := client.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refresh lease lost") {
		t.Fatalf("err = %v, want lease loss", err)
	}
	if elapsed := time.Since(start); elapsed >= 400*time.Millisecond {
		t.Errorf("exchange ran %s after the lease was lost, want abort", elapsed)
	}
	data := liveCredential(t, lease, secrets)
	// The replacement token from the aborted exchange must not be
	// persisted: another process now owns the credential.
	if strings.Contains(data, "rt-2") || !strings.Contains(data, "rt-1") {
		t.Errorf("replacement credential persisted after lease loss: %s", data)
	}
}

// TestRefreshSerializesInProcess proves one process never runs two refresh
// transactions at once: concurrent Token callers share the refresh mutex,
// so the token endpoint never sees two in-flight rotations from this
// client, and both callers get a usable token.
func TestRefreshSerializesInProcess(t *testing.T) {
	server, client, _, _, clock := tokenFixture(t, "rt-1")
	server.tokenDelay = 150 * time.Millisecond

	ctx := context.Background()
	results := make(chan string, 2)
	for range 2 {
		go func() {
			tok, err := client.Token(ctx)
			if err != nil {
				results <- "error: " + err.Error()
				return
			}
			results <- tok
		}()
	}
	for range 2 {
		got := <-results
		if got != "at-1" {
			t.Fatalf("Token = %q, want at-1", got)
		}
	}
	server.tokenReqMu.Lock()
	concur := server.tokenMaxConcur
	calls := server.tokenCalls
	server.tokenReqMu.Unlock()
	if concur > 1 {
		t.Errorf("token endpoint saw %d concurrent refreshes from one process, want at most 1", concur)
	}
	// Coalescing: the second caller rechecked under the single-flight lock
	// and reused the token the first refresh installed, so exactly one
	// rotation happened for the whole burst.
	if calls != 1 {
		t.Errorf("token requests = %d, want 1 (the burst reuses one refresh)", calls)
	}
	clock.set(clock.now.Add(time.Minute))
}

// TestRefreshForcedBypassesCoalescing pins the Refresh contract: a forced
// refresh never reuses a concurrent refresh's installed token; it performs
// its own rotation.
func TestRefreshForcedBypassesCoalescing(t *testing.T) {
	server, client, _, _, clock := tokenFixture(t, "rt-1")
	server.tokenBody = `{"access_token":"at-2","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2"}`

	tok, err := client.Refresh(context.Background())
	if err != nil || tok != "at-2" {
		t.Fatalf("Refresh = %q err=%v, want at-2", tok, err)
	}
	server.tokenReqMu.Lock()
	calls := server.tokenCalls
	server.tokenReqMu.Unlock()
	if calls != 1 {
		t.Fatalf("token requests = %d, want 1 forced rotation", calls)
	}
	clock.set(clock.now.Add(time.Minute))
}

// TestRefreshVerifiesOwnershipBeforeCredentialWrite proves the pre-write
// verification: a foreign claim that takes the lease right after ours
// fails the atomic commit gate, so the refresh aborts before any
// replacement credential is persisted.
func TestRefreshVerifiesOwnershipBeforeCredentialWrite(t *testing.T) {
	server, client, secrets, lease, clock := tokenFixture(t, "rt-1")
	server.tokenBody = `{"access_token":"at-2","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2"}`
	lease.onClaim = func() { lease.holdOther() }

	_, err := client.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "before the credential write") {
		t.Fatalf("err = %v, want pre-write ownership failure", err)
	}
	data := liveCredential(t, lease, secrets)
	if strings.Contains(data, "rt-2") {
		t.Errorf("replacement credential persisted after losing ownership: %s", data)
	}
	clock.set(clock.now.Add(time.Minute))
}

// TestRefreshFenceRejectsStaleWriter proves the persistence fence under the
// blocked-write scenario: process A's credential write blocks past its
// lease, process B claims the lease, completes its own rotation and commits
// the fence first; when A's write resumes, A's fence commit is rejected, A's
// orphan slot is deleted, and B's credential stays live — a stale writer
// never makes its value live.
func TestRefreshFenceRejectsStaleWriter(t *testing.T) {
	server := (&metadataServer{}).start(t)
	server.tokenBody = `{"access_token":"at-2","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2"}`
	server.tokenDelay = 80 * time.Millisecond
	store := newFakeSecrets()
	fence := newSharedFence()
	clock := newTestClock(time.Unix(1_700_000_000, 0))

	leaseA := newFakeLease()
	leaseA.fence = fence
	leaseB := newFakeLease()
	leaseB.fence = fence
	clientA := clientForServer(t, server, &delayedSecrets{inner: store, delay: 150 * time.Millisecond}, leaseA, clock)
	clientB := clientForServer(t, server, store, leaseB, clock)
	seedCredentials(t, store, "cid-1", "shh", server.ts.URL+"/oauth/token", server.ts.URL+"/oauth", "rt-1")

	prev := refreshLeaseTTL
	refreshLeaseTTL = 300 * time.Millisecond
	defer func() { refreshLeaseTTL = prev }()

	doneA := make(chan error, 1)
	go func() {
		_, err := clientA.Token(context.Background())
		doneA <- err
	}()
	// B claims the lease while A's exchange and write are in flight, then
	// completes its own rotation with an unblocked write.
	time.Sleep(120 * time.Millisecond)
	tokB, errB := clientB.Token(context.Background())
	if errB != nil || tokB != "at-2" {
		t.Fatalf("B Token = %q err=%v, want at-2", tokB, errB)
	}
	errA := <-doneA
	if errA == nil || !strings.Contains(errA.Error(), "superseded by a concurrent rotation") {
		t.Fatalf("A err = %v, want the stale writer rejected by the fence", errA)
	}
	if _, ok := clientA.Expiry(); ok {
		t.Fatal("the stale writer kept its token cached")
	}
	// Exactly one credential slot survived: B's. A's orphan slot and the
	// legacy label are gone.
	_, slot, found, ferr := leaseA.ReadCredentialFence(context.Background())
	if ferr != nil || !found {
		t.Fatalf("fence = found:%v err:%v", found, ferr)
	}
	data, ok, serr := store.GetSecret(slot)
	if serr != nil || !ok || !strings.Contains(string(data), "rt-2") {
		t.Fatalf("live credential = %s ok:%v err:%v", data, ok, serr)
	}
	for _, label := range store.secretLabels() {
		if label != slot && label != labelClient {
			t.Errorf("orphan credential entry %q survived", label)
		}
	}
	clock.set(clock.now.Add(time.Minute))
}

// TestTokenSkewScalesForShortLivedTokens proves a 60 s token keeps a
// positive validity window: within half its lifetime the second request is
// served from cache instead of rotating the grant again.
func TestTokenSkewScalesForShortLivedTokens(t *testing.T) {
	server, client, _, _, clock := tokenFixture(t, "rt-1")
	server.tokenBody = `{"access_token":"at-1","token_type":"Bearer","expires_in":60,"refresh_token":"rt-1"}`

	tok1, err := client.Token(context.Background())
	if err != nil || tok1 != "at-1" {
		t.Fatalf("Token = %q err=%v", tok1, err)
	}
	// 30 s in: past the fixed 60 s skew's window only if the skew were
	// not scaled, well inside the scaled one (15 s).
	clock.set(clock.now.Add(30 * time.Second))
	tok2, err := client.Token(context.Background())
	if err != nil || tok2 != "at-1" {
		t.Fatalf("cached Token = %q err=%v", tok2, err)
	}
	server.tokenReqMu.Lock()
	calls := server.tokenCalls
	server.tokenReqMu.Unlock()
	if calls != 1 {
		t.Errorf("token requests = %d, want 1 (short-lived token must stay valid past the fixed skew)", calls)
	}
	// Near expiry the token must still refresh rather than expire in use.
	clock.set(clock.now.Add(36 * time.Second))
	if _, err := client.Token(context.Background()); err != nil {
		t.Fatalf("Token near expiry: %v", err)
	}
	server.tokenReqMu.Lock()
	calls = server.tokenCalls
	server.tokenReqMu.Unlock()
	if calls != 2 {
		t.Errorf("token requests = %d, want 2 (refresh near expiry)", calls)
	}
}

// TestRefreshBlocksStaleCredentialWrite proves the generation gate: when a
// foreign process claims the refresh lease (advancing its generation)
// while this exchange is in flight, the credential write is blocked by the
// pre-write commit gate and the secret store is never touched.
func TestRefreshBlocksStaleCredentialWrite(t *testing.T) {
	server, client, inner, lease, clock := tokenFixture(t, "rt-1")
	server.tokenBody = `{"access_token":"at-2","token_type":"Bearer","expires_in":3600,"refresh_token":"rt-2"}`
	server.tokenDelay = 100 * time.Millisecond
	secrets := &recordingSecrets{fakeSecrets: inner}
	// Re-point the client's secret store at the recording wrapper. The
	// tokenFixture client already holds `inner`; rebuild is not needed
	// because the wrapper shares the same map.
	client.secrets = secrets

	go func() {
		// Let the first exchange start, then steal the lease.
		time.Sleep(30 * time.Millisecond)
		// Expire-free steal: the test drives the store directly, which is
		// what a foreign process with an expired lease would do.
		lease.mu.Lock()
		lease.holder = ""
		lease.mu.Unlock()
		_, _ = lease.ClaimLease(context.Background(), refreshLeaseName, "other-process", refreshLeaseTTL)
	}()

	_, err := client.Token(context.Background())
	if err == nil {
		t.Fatal("refresh succeeded, want the stale write blocked")
	}
	if !strings.Contains(err.Error(), "credential write") {
		t.Fatalf("err = %v, want a credential-write gate failure", err)
	}
	if secrets.sets != 0 {
		t.Fatalf("credential write ran %d times, want 0 (stale writer must not commit)", secrets.sets)
	}
	clock.set(clock.now.Add(time.Minute))
}
