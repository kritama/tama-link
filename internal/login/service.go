// Package login orchestrates Tama Link's interactive OAuth login for one
// existing profile. It owns the durable profile-scoped login lease, the
// browser or manual handoff, the bounded loopback callback, and the ordering
// that keeps a failed re-login from destroying a still-usable prior
// credential.
//
// The command layer stays thin: it parses flags, loads the profile, opens
// the profile's durable runtime, and translates this package's errors into
// stable exit status. Browser authentication and consent remain
// user-controlled; login never collects or automates credential entry.
package login

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/kritama/tama-link/internal/oauth"
	"github.com/kritama/tama-link/internal/profile"
)

const (
	// LoginLeaseName is the profile-scoped durable lease that serializes
	// interactive login attempts across processes.
	LoginLeaseName = "oauth/login"

	// DefaultCallbackDeadline is the fixed bound for one callback wait.
	DefaultCallbackDeadline = 5 * time.Minute
)

// loginLeaseTTL bounds one login lease claim. It is renewed during the
// bounded browser wait; expiry is the crash-recovery hand-off to a fresh
// attempt, so a crashed login process releases the profile automatically.
var loginLeaseTTL = 30 * time.Second

// Errors returned by this package. The command boundary translates them to
// stable, sanitized diagnostics; none carries OAuth secrets or unredacted
// upstream responses.
var (
	// ErrBusy reports that another login already holds the profile's
	// durable login lease.
	ErrBusy = errors.New("another login is already in progress for this profile")

	// ErrProfileOutdated reports that the profile predates the version 2
	// scope contract and therefore cannot be logged in.
	ErrProfileOutdated = errors.New("profile does not declare OAuth scopes")

	// ErrCallbackTimeout reports that the fixed callback deadline elapsed
	// without a terminal callback.
	ErrCallbackTimeout = errors.New("timed out waiting for the browser authorization")

	// ErrCallbackCancelled reports that the command was cancelled before
	// the terminal callback arrived.
	ErrCallbackCancelled = errors.New("login cancelled before the authorization callback arrived")
)

// Leaser coordinates the profile-scoped durable login lease. The profile
// state store satisfies this interface.
type Leaser interface {
	ClaimLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error)
	RenewLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, name, owner string) error
}

// Reporter receives the only messages login surfaces: non-sensitive
// progress notes and the manual-handoff authorization URL.
type Reporter interface {
	// AuthorizationURL presents the authorization URL for a manual handoff.
	AuthorizationURL(rawURL string)
	// Note emits one non-sensitive progress note. Notes never carry the
	// authorization URL or any attempt secret.
	Note(format string, args ...any)
}

// Options configures one login service.
type Options struct {
	// Profile is the existing validated profile the login authorizes.
	// Required.
	Profile *profile.Profile
	// Client is the profile's OAuth client, already bound to the
	// profile's resource, issuer, canonical scopes, secrets, and lease.
	// Required.
	Client *oauth.Client
	// Lease coordinates the durable profile-scoped login lease. Required.
	Lease Leaser
	// OpenBrowser opens the authorization URL in the user's browser
	// without a shell. nil selects the manual handoff directly; a launch
	// error falls back to it.
	OpenBrowser func(rawURL string) error
	// Reporter receives progress notes and the manual authorization URL.
	// Required.
	Reporter Reporter
	// CallbackDeadline bounds the callback wait. Defaults to five minutes.
	CallbackDeadline time.Duration
}

// Service runs interactive login attempts for one profile. A Service is not
// safe for concurrent use; one process runs one attempt at a time and the
// durable lease serializes the rest.
type Service struct {
	profile      *profile.Profile
	client       *oauth.Client
	lease        Leaser
	owner        string
	openBrowser  func(string) error
	reporter     Reporter
	callbackWait time.Duration
}

// CheckProfile validates that one profile can be logged in: it must be
// version 2 with a non-empty scope set. The command boundary calls it
// before opening the profile's durable runtime, so a profile-contract
// error is a usage failure and never pays the cost of a keyring or
// state-store probe.
func CheckProfile(p *profile.Profile) error {
	if p == nil {
		return errors.New("login: profile is required")
	}
	if p.Version != profile.SchemaVersion {
		return fmt.Errorf(
			"%w: profile version %d predates the scope contract; regenerate it as version 2 with a non-empty scopes array before logging in",
			ErrProfileOutdated, p.Version,
		)
	}
	if len(p.Scopes) == 0 {
		return fmt.Errorf("%w: the profile declares no OAuth scopes", ErrProfileOutdated)
	}
	return nil
}

// New validates opts and builds a Service.
func New(opts Options) (*Service, error) {
	if err := CheckProfile(opts.Profile); err != nil {
		return nil, err
	}
	if opts.Client == nil {
		return nil, errors.New("login: oauth client is required")
	}
	if opts.Lease == nil {
		return nil, errors.New("login: lease coordinator is required")
	}
	if opts.Reporter == nil {
		return nil, errors.New("login: reporter is required")
	}
	deadline := opts.CallbackDeadline
	if deadline <= 0 {
		deadline = DefaultCallbackDeadline
	}
	owner, err := newLoginOwner()
	if err != nil {
		return nil, err
	}
	return &Service{
		profile:      opts.Profile,
		client:       opts.Client,
		lease:        opts.Lease,
		owner:        owner,
		openBrowser:  opts.OpenBrowser,
		reporter:     opts.Reporter,
		callbackWait: deadline,
	}, nil
}

// newLoginOwner returns a per-process login-lease identity.
func newLoginOwner() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("login owner: %w", err)
	}
	return fmt.Sprintf("login-%d-%s", os.Getpid(), hex.EncodeToString(b[:])), nil
}

// Run executes one interactive login attempt: claim the durable login
// lease, discover and register, bind the exact loopback listener, hand the
// authorization URL to the user's browser or to the user, wait for one
// validated callback within the fixed deadline, exchange the code, and
// verify the durable credential. Every failure path closes the listener
// and releases the lease. The refresh lease is never held across the
// browser wait; the existing credential fences guard the final commit, so
// a failed re-login preserves any still-usable prior credential.
func (s *Service) Run(ctx context.Context) error {
	claimed, err := s.lease.ClaimLease(ctx, LoginLeaseName, s.owner, loginLeaseTTL)
	if err != nil {
		return fmt.Errorf("claim login lease: %w", err)
	}
	if !claimed {
		return fmt.Errorf("%w: wait for it to finish and retry", ErrBusy)
	}
	defer func() { _ = s.lease.ReleaseLease(context.WithoutCancel(ctx), LoginLeaseName, s.owner) }()

	// Renew the lease during the bounded browser wait: expiry recovers a
	// crash, but a live attempt must not lose its lease mid-wait. A lost
	// lease cancels the working context and aborts the attempt before any
	// credential mutation.
	execCtx, cancelExec := context.WithCancel(ctx)
	renewed := make(chan struct{})
	go s.renewLoginLease(execCtx, cancelExec, renewed)
	defer func() {
		// Cancel first: the renewal goroutine exits only when the working
		// context ends, and the join must not run before the cancel.
		cancelExec()
		<-renewed
	}()

	md, err := s.client.Discover(execCtx)
	if err != nil {
		return fmt.Errorf("discover oauth metadata: %w", err)
	}
	if err := md.CheckRequestedScopes(s.profile.Scopes); err != nil {
		return fmt.Errorf("validate requested scopes: %w", err)
	}
	rec, err := s.client.Register(execCtx, md)
	if err != nil {
		return fmt.Errorf("register oauth client: %w", err)
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind loopback listener: %w", err)
	}
	defer func() { _ = listener.Close() }()
	redirectURI := fmt.Sprintf("http://%s/oauth/callback", listener.Addr().String())

	authReq, err := s.client.NewAuthorizationRequest(md, rec, redirectURI)
	if err != nil {
		return err
	}

	s.handoff(authReq.URL.String())

	cb, err := NewCallback(listener, authReq.State, redirectURI, s.callbackIssuer(md))
	if err != nil {
		return fmt.Errorf("arm loopback callback: %w", err)
	}
	outcome, err := cb.Wait(execCtx, s.callbackWait)
	if err != nil {
		return err
	}
	if outcome.OAuthError != "" {
		return fmt.Errorf("authorization failed: the authorization server reported %s", outcome.OAuthError)
	}
	if err := s.client.CompleteAuthorization(execCtx, md, rec, authReq, outcome.Code, redirectURI); err != nil {
		return fmt.Errorf("complete authorization: %w", err)
	}
	// Verify the durable credential before reporting success: the access
	// token dies with this process, the refresh credential is the result.
	ready, err := s.client.HasCredentials(execCtx)
	if err != nil {
		return fmt.Errorf("verify committed credential: %w", err)
	}
	if !ready {
		return errors.New("login committed but the durable credential is missing; run login again")
	}
	return nil
}

// callbackIssuer returns the value the callback must echo in the iss
// parameter when discovery says the server supports the authorization
// response issuer, and the empty string when it does not.
func (s *Service) callbackIssuer(md *oauth.Metadata) string {
	if md.IssuerResponseRequired() {
		return md.AS.Issuer
	}
	return ""
}

// renewLoginLease renews the login lease on a third of its TTL until the
// caller's context is cancelled. A renewal that fails or reports lost
// ownership cancels the working context so the attempt aborts before any
// credential mutation.
func (s *Service) renewLoginLease(ctx context.Context, cancel context.CancelFunc, done chan<- struct{}) {
	defer close(done)
	interval := loginLeaseTTL / 3
	if interval <= 0 {
		interval = loginLeaseTTL
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			renewCtx, cancelRenew := context.WithTimeout(ctx, interval)
			owned, err := s.lease.RenewLease(renewCtx, LoginLeaseName, s.owner, loginLeaseTTL)
			cancelRenew()
			if ctx.Err() != nil {
				return
			}
			if err != nil || !owned {
				cancel()
				return
			}
			timer.Reset(interval)
		}
	}
}
