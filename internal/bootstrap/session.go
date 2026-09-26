package bootstrap

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/upstream"
)

const (
	// LeaseName is the profile-scoped durable claim that serializes bootstrap
	// across processes. It is not an in-memory lock.
	LeaseName = "bootstrap/create"

	leaseTTL = 30 * time.Second
)

// Session is the profile-isolated runtime opened for one bootstrap name.
// Authorization, credential readiness, and the durable lease live here so
// bootstrap never copies secret values between namespaces.
type Session interface {
	Close() error
	Authorize(ctx context.Context, noBrowser bool) error
	Token(ctx context.Context) (string, error)
	Ready(ctx context.Context) (bool, error)
	Logout(ctx context.Context) error
	ClaimLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error)
	RenewLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, name, owner string) error
	// LeaseGeneration returns the generation of the lease owner currently holds.
	LeaseGeneration(ctx context.Context, name, owner string) (int64, bool, error)
	// CommitLease is the durable generation gate immediately before an
	// irreversible publication. It reports false when owner no longer holds
	// that generation.
	CommitLease(ctx context.Context, name, owner string, generation int64) (bool, error)
	DatabasePath() string
}

// Options configures one bootstrap attempt.
type Options struct {
	ConfigDir     string
	ConfigDirSet  bool
	Interactive   bool
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
	HTTP          *http.Client
	OpenSession   func(ctx context.Context, shell *profile.Profile) (Session, error)
	ReadCatalog   func(ctx context.Context, endpoint, token string) (*upstream.DiscoverResult, []*upstream.LiveTool, error)
	LoginExisting func(ctx context.Context, p *profile.Profile) error
}

// Request is the explicit login input. A set flag was supplied by the user
// and suppresses the matching prompt. An omitted flag is not user input.
type Request struct {
	Address    string
	AddressSet bool
	Type       string
	TypeSet    bool
	Profile    string
	ProfileSet bool
	Issuer     string
	IssuerSet  bool
	Yes        bool
	NoBrowser  bool
}

// Result is a completed login. Created is false for an existing profile.
type Result struct {
	Name         profile.Name
	Created      bool
	ServeCommand string
}
