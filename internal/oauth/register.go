package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kritama/tama-link/internal/store"
)

// Secret labels inside the profile credential namespace.
const (
	labelClient  = "oauth-client"
	labelRefresh = "oauth-refresh"
)

// ClientRecord is the persisted dynamic client registration. The secret,
// when present, lives only in the credential backend.
type ClientRecord struct {
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret,omitempty"`
	AuthMethod   string    `json:"token_endpoint_auth_method"`
	Issuer       string    `json:"issuer"`
	RegisteredAt time.Time `json:"registered_at"`
	// SecretExpiresAt is the RFC 7591 client_secret_expires_at: a Unix
	// second after which the secret is invalid, or zero when the secret
	// does not expire.
	SecretExpiresAt int64 `json:"client_secret_expires_at,omitempty"`
}

// secretExpired reports whether the record's client secret has passed its
// RFC 7591 expiry. An expired secret authenticates no exchange, so the
// record must not count as a usable registration.
func (c *Client) secretExpired(rec *ClientRecord) bool {
	return rec.SecretExpiresAt > 0 && c.clock().Unix() >= rec.SecretExpiresAt
}

// RegisteredClient returns the stored client registration, or found=false.
// A legacy record that predates the per-method secret check and lacks the
// secret its auth method requires is treated as absent: refresh and
// readiness fail as no-credential, and the next login re-registers, so an
// upgraded profile self-heals instead of looping on an unusable record.
func (c *Client) RegisteredClient(ctx context.Context) (*ClientRecord, bool, error) {
	data, found, err := c.loadStoredClient(ctx)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	var rec ClientRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, false, fmt.Errorf("stored client record is malformed: json")
	}
	if rec.ClientID == "" || rec.Issuer == "" || rec.AuthMethod == "" {
		return nil, false, fmt.Errorf("stored client record is incomplete")
	}
	if recordMissingSecret(&rec) || c.secretExpired(&rec) {
		return nil, false, nil
	}
	return &rec, true, nil
}

// loadStoredClient returns the live client registration record: the
// fenced slot when a client fence has been committed, otherwise the
// legacy single-label form. found=false means no usable record.
func (c *Client) loadStoredClient(ctx context.Context) ([]byte, bool, error) {
	_, slot, found, err := c.lease.ReadCredentialFence(ctx, store.ClientFenceName)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	if found {
		data, ok, err := c.secrets.GetSecret(slot)
		if err != nil {
			return nil, false, fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
		}
		if !ok {
			return nil, false, fmt.Errorf("%w: client record slot is missing", ErrBackendUnavailable)
		}
		return data, true, nil
	}
	data, found, err := c.secrets.GetSecret(labelClient)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrBackendUnavailable, err)
	}
	return data, found, nil
}

// recordMissingSecret reports whether the record's auth method requires a
// client secret the record does not carry.
func recordMissingSecret(rec *ClientRecord) bool {
	return (rec.AuthMethod == "client_secret_basic" || rec.AuthMethod == "client_secret_post") &&
		rec.ClientSecret == ""
}

// currentRecord re-reads the stored client registration and requires it to
// still be the record the login flow started from, with a usable secret.
// A secret can expire, or the registration be replaced, while the user
// completes browser consent; an in-memory record outlives neither, so an
// exchange or commit against it must fail instead of authenticating a
// superseded identity.
func (c *Client) currentRecord(ctx context.Context, rec *ClientRecord) error {
	stored, found, err := c.RegisteredClient(ctx)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: the stored client registration is no longer usable", ErrNoCredentials)
	}
	if stored.ClientID != rec.ClientID || stored.ClientSecret != rec.ClientSecret || stored.Issuer != rec.Issuer {
		return fmt.Errorf("%w: the client registration was replaced during the login", ErrNoCredentials)
	}
	return nil
}

// Register returns the stored registration when it still binds the same
// issuer, otherwise performs dynamic client registration and persists the
// result.
func (c *Client) Register(ctx context.Context, md *Metadata) (*ClientRecord, error) {
	if rec, found, err := c.RegisteredClient(ctx); err != nil {
		return nil, err
	} else if found {
		if rec.Issuer != md.AS.Issuer {
			return nil, fmt.Errorf("%w: stored client registration binds a different issuer", ErrMetadata)
		}
		return rec, nil
	}

	body := map[string]any{
		"client_name":                "Tama Link",
		"redirect_uris":              []string{c.redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": md.AS.TokenEndpointAuthMethod(),
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode registration: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, md.AS.RegistrationEndpoint, strings.NewReader(string(data)))
	if err != nil {
		return nil, fmt.Errorf("build registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("register client: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := readBody(resp.Body, c.maxBytes)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registration endpoint answered %d", resp.StatusCode)
	}
	var created struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		// Zero means the secret does not expire (RFC 7591).
		SecretExpiresAt int64 `json:"client_secret_expires_at"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		return nil, fmt.Errorf("decode registration: json")
	}
	if created.ClientID == "" {
		return nil, fmt.Errorf("registration returned no client id")
	}
	rec := &ClientRecord{
		ClientID:        created.ClientID,
		ClientSecret:    created.ClientSecret,
		AuthMethod:      md.AS.TokenEndpointAuthMethod(),
		Issuer:          md.AS.Issuer,
		RegisteredAt:    c.clock().UTC(),
		SecretExpiresAt: created.SecretExpiresAt,
	}
	// A client-secret auth method without a secret would be persisted as a
	// permanently unusable registration: readiness would accept submits
	// that burn idempotency keys, and every token exchange would fail
	// locally on the missing secret. The same check re-validates stored
	// records on load, so a legacy record self-heals on the next login.
	if recordMissingSecret(rec) {
		return nil, fmt.Errorf("registration returned no client secret for %s", rec.AuthMethod)
	}
	// The replacement is one credential-mutation protocol. The local
	// lock orders it against this process's own completion, refresh, and
	// logout — a same-owner claim succeeds without advancing the epoch,
	// so the lease alone cannot order them — and the cross-process lease
	// orders it against every other process. A replacement registration
	// issues a new client ID, so any refresh credential still stored
	// belongs to the previous client and can never refresh under the new
	// one: retiring it and committing the new record under the same
	// claimed epoch means a login started from the previous record either
	// commits its grant before the replacement (the grant is then retired
	// as orphaned) or aborts against the superseded record — never pairs
	// the new client with a grant issued under the old one. The normal
	// first-login path stores no credential yet and the retirement is a
	// no-op.
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	leaseGeneration, claimed, err := c.claimRefreshLease(ctx)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, fmt.Errorf("%w: the credential is being mutated; retry registration", ErrLeaseContention)
	}
	defer func() { _ = c.lease.ReleaseLease(context.WithoutCancel(ctx), refreshLeaseName, c.owner) }()

	// Renew ownership across the whole mutation on a context that
	// outlives the caller's cancellation: the final record write takes
	// no context and cannot be aborted, so if the secure backend blocks
	// beyond the lease TTL the renewal is the only thing keeping the
	// epoch alive, and a caller cancellation during that write must not
	// hand the epoch to another process. Every interruptible step still
	// observes the caller context directly.
	renewCtx, cancelExec := context.WithCancel(context.WithoutCancel(ctx))
	renewed := make(chan struct{})
	go c.renewLease(renewCtx, cancelExec, renewed)
	defer func() {
		cancelExec()
		<-renewed
	}()

	if existing, err := c.loadFenced(ctx); err != nil {
		return nil, err
	} else if existing != nil {
		if err := c.invalidateCredential(ctx, leaseGeneration); err != nil {
			return nil, err
		}
	}
	// The fence commit is atomic against the claimed epoch: a writer
	// that loses the lease while its slot write is blocked is rejected
	// and rolls back its own slot, so no optimistic pre- or post-write
	// check is needed.
	if err := c.storeClient(ctx, rec, leaseGeneration); err != nil {
		return nil, err
	}
	return rec, nil
}

// storeClient commits one client registration through the client fence,
// the same protocol as the refresh credential: the record goes to a
// unique secure-backend slot, and the fence pointer — advanced atomically
// against the claimed lease epoch — makes it live. A writer that lost the
// epoch, or was passed by a concurrent commit, has its advance rejected
// and removes its own slot; a failed rollback is recorded in the durable
// retirement backlog. It can never install or remove another writer's record.
// The caller holds the refresh lease (epoch leaseGeneration).
func (c *Client) storeClient(ctx context.Context, rec *ClientRecord, leaseGeneration int64) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode client record: %w", err)
	}
	slot, err := newClientSlotLabel()
	if err != nil {
		return err
	}
	if err := c.secrets.SetSecret(slot, data); err != nil {
		// A backend may report an uncertain write. Roll back the unique slot;
		// if deletion also fails, keep a durable cleanup record.
		return fmt.Errorf("%w: %w", ErrBackendUnavailable,
			errors.Join(err, c.rollbackClientSlot(ctx, slot)))
	}
	generation, previous, found, err := c.lease.ReadCredentialFence(ctx, store.ClientFenceName)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable,
			errors.Join(err, c.rollbackClientSlot(ctx, slot)))
	}
	fenceGeneration := int64(1)
	if found {
		fenceGeneration = generation + 1
	}
	committed, err := c.lease.CommitCredentialFence(ctx, store.CredentialFenceCommit{
		FenceName:       store.ClientFenceName,
		FenceGeneration: fenceGeneration,
		Slot:            slot,
		PreviousSlot:    previous,
		LeaseName:       refreshLeaseName,
		LeaseOwner:      c.owner,
		LeaseGeneration: leaseGeneration,
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBackendUnavailable,
			errors.Join(err, c.rollbackClientSlot(ctx, slot)))
	}
	if !committed {
		// This writer lost the epoch or was passed by a concurrent
		// commit: the slot is referenced by no fence, so removing it
		// cannot touch another writer's record.
		if rollbackErr := c.rollbackClientSlot(ctx, slot); rollbackErr != nil {
			return fmt.Errorf("%w: %w", ErrLeaseContention, rollbackErr)
		}
		return fmt.Errorf("%w: the client record commit was rejected", ErrLeaseContention)
	}
	// The legacy single-label record is dead data once a client fence
	// exists — reads take the fenced slot and fall back only when no
	// fence has been committed. Retire it so a record treated as absent
	// on load (an expired or missing secret) does not linger in the
	// backend. Best effort: the commit is already durable, so a deletion
	// failure is durably recorded for the retirement sweep instead of
	// failing the registration it follows.
	if err := c.secrets.DeleteSecret(labelClient); err != nil {
		_ = c.lease.RecordRetiredCredentialSlot(
			context.WithoutCancel(ctx), store.ClientFenceName, labelClient,
		)
	}
	return nil
}

// rollbackClientSlot removes a client-registration slot that never became
// live. A secure-backend deletion can fail after the slot write succeeded;
// record that slot durably on a cancellation-independent context so a later
// refresh or logout can retry it instead of orphaning a client secret.
func (c *Client) rollbackClientSlot(ctx context.Context, slot string) error {
	if err := c.secrets.DeleteSecret(slot); err != nil {
		recordErr := c.lease.RecordRetiredCredentialSlot(
			context.WithoutCancel(ctx), store.ClientFenceName, slot,
		)
		if recordErr != nil {
			return errors.Join(
				fmt.Errorf("delete uncommitted client slot: %w", err),
				fmt.Errorf("record uncommitted client slot for retirement: %w", recordErr),
			)
		}
		return fmt.Errorf("delete uncommitted client slot: %w", err)
	}
	return nil
}

// TokenEndpointAuthMethod returns the auth method this client will use:
// the selection discovery validated, shared with registration.
func (as *AuthorizationServer) TokenEndpointAuthMethod() string {
	return tokenEndpointAuthMethod(as.TokenEndpointAuthMethods)
}

// applyFormAuth adds the form-based client credentials for the auth method
// (RFC 7591 public clients send client_id; client_secret_post sends both).
// It must run before the form is encoded into the request body; the client
// secret never appears outside this function and applyHeaderAuth.
func (rec *ClientRecord) applyFormAuth(form url.Values) error {
	switch rec.AuthMethod {
	case "none":
		form.Set("client_id", rec.ClientID)
	case "client_secret_post":
		if rec.ClientSecret == "" {
			return fmt.Errorf("client secret missing for client_secret_post")
		}
		form.Set("client_id", rec.ClientID)
		form.Set("client_secret", rec.ClientSecret)
	case "client_secret_basic":
		// The credentials ride in the Authorization header instead.
	default:
		return fmt.Errorf("unsupported token endpoint auth method %q", rec.AuthMethod)
	}
	return nil
}

// applyHeaderAuth applies the header-based credentials for the auth method
// after the request is constructed.
func (rec *ClientRecord) applyHeaderAuth(req *http.Request) error {
	if rec.AuthMethod != "client_secret_basic" {
		return nil
	}
	if rec.ClientSecret == "" {
		return fmt.Errorf("client secret missing for client_secret_basic")
	}
	req.SetBasicAuth(rec.ClientID, rec.ClientSecret)
	return nil
}

// tokenResponse is the RFC 6749 token endpoint response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

// postToken performs one token endpoint exchange and decodes the response.
// The provider's error description is deliberately not carried into the
// returned error: it is provider text that could embed request data.
func (c *Client) postToken(ctx context.Context, endpoint string, rec *ClientRecord, form url.Values) (*tokenResponse, error) {
	// Form-based credentials (none, client_secret_post) must be added before
	// the body is encoded; header-based credentials (client_secret_basic)
	// are applied after the request exists.
	if err := rec.applyFormAuth(form); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if err := rec.applyHeaderAuth(req); err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := readBody(resp.Body, c.maxBytes)
	if err != nil {
		return nil, err
	}
	var out tokenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode token response: json")
	}
	if out.AccessToken != "" {
		// The upstream transport unconditionally sends the access token as
		// a Bearer credential: an omitted or different token type would
		// pass readiness while every authenticated call fails.
		if !strings.EqualFold(out.TokenType, "bearer") {
			return nil, fmt.Errorf("token endpoint returned unsupported token type %q", out.TokenType)
		}
		return &out, nil
	}
	if out.Error == "" {
		return nil, fmt.Errorf("token endpoint answered %d with no token", resp.StatusCode)
	}
	if out.Error == "invalid_grant" {
		return nil, fmt.Errorf("%w", ErrGrantInvalid)
	}
	return nil, fmt.Errorf("token endpoint rejected the request: %s", out.Error)
}
