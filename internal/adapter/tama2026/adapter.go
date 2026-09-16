package tama2026

import (
	"context"
	"fmt"

	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/upstream"
)

// protocolVersion is the only MCP protocol version this adapter speaks.
const protocolVersion = "2026-07-28"

// Adapter verifies one profile's pinned catalog against one TamaMCP 2026
// upstream endpoint. It owns no transport and no credentials of its own:
// the upstream client already carries the profile OAuth token provider.
type Adapter struct {
	profile        profile.Profile
	upstream       *upstream.Client
	adapterVersion string
}

// Config wires one adapter for one process profile.
type Config struct {
	// Profile is the validated non-secret profile. Required.
	Profile profile.Profile
	// Upstream is the stateless 2026-07-28 client bound to the profile
	// endpoint with the profile OAuth token provider. Required.
	Upstream *upstream.Client
	// AdapterVersion identifies this adapter build; it is recorded on
	// accepted submissions for compatibility auditing. Required.
	AdapterVersion string
}

// New validates cfg and builds an Adapter.
func New(cfg Config) (*Adapter, error) {
	if cfg.Profile.Endpoint == "" {
		return nil, fmt.Errorf("profile endpoint is required")
	}
	if cfg.Upstream == nil {
		return nil, fmt.Errorf("upstream client is required")
	}
	if cfg.Upstream.Endpoint() != cfg.Profile.Endpoint {
		return nil, fmt.Errorf("upstream client is bound to a different endpoint")
	}
	if cfg.AdapterVersion == "" {
		return nil, fmt.Errorf("adapter version is required")
	}
	if !cfg.Profile.Bounds.Contains(protocolVersion) {
		return nil, fmt.Errorf("profile bounds %s to %s do not cover protocol %s", cfg.Profile.Bounds.ProtocolMin, cfg.Profile.Bounds.ProtocolMax, protocolVersion)
	}
	if err := cfg.Profile.Catalog().Validate(); err != nil {
		return nil, fmt.Errorf("profile catalog: %w", err)
	}
	return &Adapter{
		profile:        cfg.Profile,
		upstream:       cfg.Upstream,
		adapterVersion: cfg.AdapterVersion,
	}, nil
}

// Connect authenticates, discovers, and verifies the pinned catalog. The
// returned connection is the only path to upstream execution for this
// profile; every verification below fails closed.
func (a *Adapter) Connect(ctx context.Context) (*Connection, error) {
	disc, err := a.upstream.Discover(ctx)
	if err != nil {
		return nil, classify(err)
	}
	if err := a.verifyDiscovery(disc); err != nil {
		return nil, err
	}
	live, err := a.upstream.ListAllTools(ctx)
	if err != nil {
		return nil, classify(err)
	}
	effective, err := a.verifyCatalog(disc, live)
	if err != nil {
		return nil, err
	}
	serverName, serverVersion := "", ""
	if disc.ServerInfo != nil {
		serverName = disc.ServerInfo.Name
		serverVersion = disc.ServerInfo.Version
	}
	return &Connection{
		adapter:            a,
		protocolVersion:    protocolVersion,
		serverName:         serverName,
		serverVersion:      serverVersion,
		serverCapabilities: disc.Capabilities,
		taskExtension:      disc.HasTaskExtension(),
		catalog:            effective,
	}, nil
}
