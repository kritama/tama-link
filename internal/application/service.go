package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/worker"
)

// Config wires the application service for one validated profile.
type Config struct {
	// Profile is the validated process profile. Required.
	Profile *profile.Profile
	// Store is the profile state database. Required.
	Store *store.Store
	// Connect supplies one verified upstream connection. The service calls
	// it once and reuses the result for the process lifetime; every
	// upstream request goes through that connection. Required.
	Connect func(ctx context.Context) (*tama2026.Connection, error)
	// Worker executes local_replayable submissions under the durable lease.
	// Required.
	Worker *worker.Service
	// AdapterVersion identifies the adapter build recorded on accepted
	// submissions. Required.
	AdapterVersion string
	// Now supplies the clock. The zero value uses time.Now.
	Now func() time.Time
}

// Service is one profile's submit/await application service. It is safe for
// concurrent use.
type Service struct {
	profile        *profile.Profile
	store          *store.Store
	connect        func(ctx context.Context) (*tama2026.Connection, error)
	worker         *worker.Service
	adapterVersion string
	now            func() time.Time
}

// New validates cfg and builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Profile == nil {
		return nil, errors.New("profile is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("store is required")
	}
	if cfg.Connect == nil {
		return nil, errors.New("connect function is required")
	}
	if cfg.Worker == nil {
		return nil, errors.New("worker is required")
	}
	if cfg.AdapterVersion == "" {
		return nil, errors.New("adapter version is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		profile:        cfg.Profile,
		store:          cfg.Store,
		connect:        cfg.Connect,
		worker:         cfg.Worker,
		adapterVersion: cfg.AdapterVersion,
		now:            now,
	}, nil
}

// newSubmissionID returns one opaque Tama Link submission identifier.
func newSubmissionID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate submission id: %w", err)
	}
	return "sub_" + hex.EncodeToString(b[:]), nil
}

// failed renders one stable request-level error.
func failed(code contract.Code, format string, args ...any) *contract.Error {
	e := contract.NewError(code, fmt.Sprintf(format, args...))
	return &e
}
