package main

import (
	"context"
	"fmt"

	"github.com/kritama/tama-link/internal/credential"
	"github.com/kritama/tama-link/internal/login"
	"github.com/kritama/tama-link/internal/oauth"
	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/store"
)

// profileRuntime is one profile's durable runtime dependencies: the secure
// credential backend, the state store, and the OAuth client. serve and
// login open the same canonical profile-scoped locations through it.
type profileRuntime struct {
	Keyring *credential.Keyring
	Store   *store.Store
	OAuth   *oauth.Client
}

// Close releases the runtime's state store. The credential backend is
// process-scoped and has no close.
func (rt *profileRuntime) Close() { _ = rt.Store.Close() }

// openProfileRuntime opens one profile's secure credential namespace, state
// store, and OAuth client at the canonical profile-scoped locations.
func openProfileRuntime(ctx context.Context, p *profile.Profile, configDir string, hooks serveHooks) (*profileRuntime, error) {
	dbPath, namespace, err := stateLayout(p, configDir)
	if err != nil {
		return nil, err
	}
	if hooks.open == nil {
		hooks.open = credential.New
	}
	kr, err := hooks.open(namespace)
	if err != nil {
		return nil, fmt.Errorf("open credential backend: %w", err)
	}
	st, err := store.Open(ctx, dbPath, kr, store.Config{Limits: p.EffectiveLimits()})
	if err != nil {
		return nil, fmt.Errorf("open state store: %w", err)
	}
	client, err := oauth.New(oauth.Config{
		Endpoint: p.Endpoint,
		Issuer:   p.Issuer,
		// The registration carries the native-loopback base plus the fixed
		// callback path: the port varies per attempt, but a provider that
		// compares paths must see the one every authorization uses.
		RedirectURI: "http://127.0.0.1" + login.CallbackPath,
		Scopes:      p.Scopes,
		Secrets:     kr,
		Lease:       st,
		HTTPClient:  hooks.client(),
	})
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("configure oauth: %w", err)
	}
	return &profileRuntime{Keyring: kr, Store: st, OAuth: client}, nil
}
