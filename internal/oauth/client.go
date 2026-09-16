package oauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/kritama/tama-link/internal/store"
)

// Default bounds for one OAuth client.
const (
	// DefaultMaxMetadataBytes bounds one metadata or token response body.
	DefaultMaxMetadataBytes = 256 << 10

	// refreshLeaseName is the profile-scoped cross-process refresh lease.
	refreshLeaseName = "oauth/refresh"

	// refreshSkew refreshes a little before the recorded expiry so a
	// request is never issued with an expired token.
	refreshSkew = 60 * time.Second
)

// refreshLeaseTTL bounds one refresh transaction; the lease is renewed on
// a third of it while the token exchange and the credential write run.
var refreshLeaseTTL = 30 * time.Second

// SecretStore persists profile-scoped OAuth secrets in the secure credential
// backend. Labels are opaque non-secret identifiers.
type SecretStore interface {
	// GetSecret returns the secret stored under label, or found=false.
	GetSecret(label string) (data []byte, found bool, err error)
	// SetSecret stores data under label, replacing any previous value.
	SetSecret(label string, data []byte) error
	// DeleteSecret removes label. Absent labels are not an error.
	DeleteSecret(label string) error
}

// Leaser coordinates the profile-scoped cross-process refresh lease. The
// profile state store satisfies this interface.
type Leaser interface {
	ClaimLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error)
	RenewLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, name, owner string) error
	// LeaseGeneration returns the generation of the held lease, or ok=false
	// when owner does not hold it.
	LeaseGeneration(ctx context.Context, name, owner string) (int64, bool, error)
	// CommitLease atomically verifies that owner still holds the lease in
	// the given generation. The refresh path uses it as a cheap pre-write
	// gate: a lease lost to a foreign claim blocks the stale write.
	CommitLease(ctx context.Context, name, owner string, generation int64) (bool, error)
	// ReadCredentialFence returns the generation and secure-backend slot
	// the fence currently points at, or found=false when no fenced
	// credential has been committed yet.
	ReadCredentialFence(ctx context.Context) (generation int64, slot string, found bool, err error)
	// CommitCredentialFence atomically advances the fence to commit.Slot
	// and enqueues commit.PreviousSlot for retirement retry when, and only
	// when, the fence has not advanced past commit.FenceGeneration and the
	// caller still holds the lease epoch in the commit. It is the
	// credential-side compare-and-swap that a stale writer cannot pass;
	// the enqueue is one transaction with the advance, so a committed
	// fence always carries a durable retirement record for the slot it
	// replaced.
	CommitCredentialFence(ctx context.Context, commit store.CredentialFenceCommit) (bool, error)
	// ClearCredentialFence removes the credential fence when, and only
	// when, leaseOwner still holds leaseName unexpired in the lease
	// ownership epoch leaseGeneration. An absent fence is successfully
	// cleared. It reports cleared=false for a lost epoch, so a logout
	// whose lease was lost mid-cleanup can never wipe a newer fence
	// installed by the process that took over.
	ClearCredentialFence(ctx context.Context, leaseName, leaseOwner string, leaseGeneration int64) (bool, error)
	// RetiredCredentialSlots lists the credential slots recorded for
	// retirement retry.
	RetiredCredentialSlots(ctx context.Context) ([]string, error)
	// ClearRetiredCredentialSlot removes the retirement record for one
	// slot after its deletion succeeded.
	ClearRetiredCredentialSlot(ctx context.Context, slot string) error
}

// Config configures one profile's OAuth client.
type Config struct {
	// Endpoint is the profile's upstream MCP endpoint. It is the exact
	// RFC 8707 resource bound to every token request and the base for
	// protected-resource discovery. Required.
	Endpoint string
	// Issuer is the expected authorization-server issuer, exactly.
	// Required.
	Issuer string
	// RedirectURI is the loopback URI registered with the authorization
	// server during dynamic client registration. Defaults to
	// "http://127.0.0.1" (any ephemeral port). The actual listener port is
	// selected by the caller before NewAuthorizationRequest, which carries
	// the exact URI through the authorization and token exchanges.
	RedirectURI string
	// Secrets stores OAuth secrets in the profile credential namespace.
	// Required.
	Secrets SecretStore
	// Lease coordinates refresh across processes. Required.
	Lease Leaser
	// Clock is injectable for deterministic expiry tests. Defaults to
	// time.Now.
	Clock func() time.Time
	// HTTPClient is optional. Redirects are always rejected.
	HTTPClient *http.Client
	// MaxMetadataBytes bounds one metadata or token response body.
	// Defaults to DefaultMaxMetadataBytes.
	MaxMetadataBytes int64
}

// Client is one profile's OAuth client. It holds the in-memory access token
// and coordinates refresh through the profile lease. A Client is safe for
// concurrent use.
type Client struct {
	endpoint    string
	issuer      string
	redirectURI string
	secrets     SecretStore
	lease       Leaser
	clock       func() time.Time
	httpClient  *http.Client
	maxBytes    int64
	owner       string

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
	hasToken    bool
	// skew is the refresh lead time for the held token, capped to its
	// lifetime; applyTokens sets it before the token becomes visible.
	skew time.Duration

	// refreshMu serializes refresh transactions inside this process; the
	// cross-process lease is shared by every in-process refresher.
	refreshMu sync.Mutex
}

// New validates cfg and builds a Client.
func New(cfg Config) (*Client, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("endpoint is required")
	}
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}
	if err := checkSecureURL(endpoint); err != nil {
		return nil, fmt.Errorf("endpoint: %w", err)
	}
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("expected issuer is required")
	}
	if cfg.Secrets == nil {
		return nil, fmt.Errorf("secret store is required")
	}
	if cfg.Lease == nil {
		return nil, fmt.Errorf("lease coordinator is required")
	}
	if cfg.RedirectURI == "" {
		cfg.RedirectURI = "http://127.0.0.1"
	}
	redirect, err := url.Parse(cfg.RedirectURI)
	if err != nil || redirect.Hostname() != "127.0.0.1" || redirect.Scheme != "http" {
		return nil, fmt.Errorf("redirect uri must be an http 127.0.0.1 loopback uri")
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	maxBytes := cfg.MaxMetadataBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxMetadataBytes
	}
	// Redirects are refused on every client, default or supplied: metadata,
	// registration, and token destinations must all come from validated
	// profile metadata, never from a Location header. The supplied client is
	// cloned so the refusal never mutates caller state.
	base := cfg.HTTPClient
	if base == nil {
		base = &http.Client{Timeout: 30 * time.Second}
	}
	cloned := *base
	cloned.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	httpClient := &cloned
	owner, err := newOwner()
	if err != nil {
		return nil, err
	}
	return &Client{
		endpoint:    endpoint.String(),
		issuer:      cfg.Issuer,
		redirectURI: redirect.String(),
		secrets:     cfg.Secrets,
		lease:       cfg.Lease,
		clock:       clock,
		httpClient:  httpClient,
		maxBytes:    maxBytes,
		owner:       owner,
	}, nil
}

// newOwner returns a per-process lease owner identity.
func newOwner() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("oauth owner: %w", err)
	}
	return fmt.Sprintf("oauth-%d-%s", os.Getpid(), hex.EncodeToString(b[:])), nil
}
