package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kritama/tama-link/internal/bootstrap"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/login"
	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/upstream"
	"github.com/kritama/tama-link/internal/version"
)

// loginSession is one bootstrap runtime. It adapts the existing login
// service and profile store without copying credential material.
type loginSession struct {
	rt      *profileRuntime
	profile *profile.Profile
	stdout  io.Writer
	stderr  io.Writer
	dbPath  string
}

func openBootstrapSession(ctx context.Context, shell *profile.Profile, configDir string, stdout, stderr io.Writer) (bootstrap.Session, error) {
	rt, err := openProfileRuntime(ctx, shell, configDir, fixtureHooks())
	if err != nil {
		return nil, err
	}
	dbPath, _, err := stateLayout(shell, configDir)
	if err != nil {
		rt.Close()
		return nil, err
	}
	return &loginSession{rt: rt, profile: shell, stdout: stdout, stderr: stderr, dbPath: dbPath}, nil
}

func (s *loginSession) Close() error {
	s.rt.Close()
	return nil
}

func (s *loginSession) Authorize(ctx context.Context, noBrowser bool) error {
	svc, err := login.New(login.Options{
		Profile:     s.profile,
		Client:      s.rt.OAuth,
		Lease:       s.rt.Store,
		OpenBrowser: loginOpenBrowser(noBrowser),
		Reporter:    loginReporter{stdout: s.stdout, stderr: s.stderr},
	})
	if err != nil {
		return err
	}
	return svc.Run(ctx)
}

func (s *loginSession) Token(ctx context.Context) (string, error) {
	return s.rt.OAuth.Token(ctx)
}

func (s *loginSession) Ready(ctx context.Context) (bool, error) {
	return s.rt.OAuth.HasCredentials(ctx)
}

func (s *loginSession) Logout(ctx context.Context) error {
	return s.rt.OAuth.Logout(ctx)
}

func (s *loginSession) ClaimLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	return s.rt.Store.ClaimLease(ctx, name, owner, ttl)
}

func (s *loginSession) RenewLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	return s.rt.Store.RenewLease(ctx, name, owner, ttl)
}

func (s *loginSession) ReleaseLease(ctx context.Context, name, owner string) error {
	return s.rt.Store.ReleaseLease(ctx, name, owner)
}

func (s *loginSession) LeaseGeneration(ctx context.Context, name, owner string) (int64, bool, error) {
	return s.rt.Store.LeaseGeneration(ctx, name, owner)
}

func (s *loginSession) CommitLease(ctx context.Context, name, owner string, generation int64) (bool, error) {
	return s.rt.Store.CommitLease(ctx, name, owner, generation)
}

func (s *loginSession) DatabasePath() string { return s.dbPath }

func readBootstrapCatalog(ctx context.Context, endpoint, token string) (*upstream.DiscoverResult, []*upstream.LiveTool, error) {
	bound := int64(limits.HardCeiling().ResponseBytes)
	client, err := upstream.New(upstream.Config{
		Endpoint: endpoint,
		ClientInfo: mcp.Implementation{
			Name:    "tama-link",
			Version: version.Version,
		},
		ClientCapabilities: json.RawMessage(`{"extensions":{"io.modelcontextprotocol/tasks":{}}}`),
		TokenProvider: func(context.Context) (string, error) {
			return token, nil
		},
		MaxResponseBytes: bound,
		HTTPClient:       fixtureHooks().client(),
	})
	if err != nil {
		return nil, nil, err
	}
	disc, err := client.Discover(ctx, bound)
	if err != nil {
		return nil, nil, err
	}
	live, err := client.ListAllTools(ctx, bound)
	if err != nil {
		return nil, nil, err
	}
	return disc, live, nil
}

func loginExit(err error) (*bootstrap.ExitError, bool) {
	var exit *bootstrap.ExitError
	if errors.As(err, &exit) {
		return exit, true
	}
	return nil, false
}
