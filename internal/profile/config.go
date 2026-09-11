package profile

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/limits"
)

const (
	// SchemaVersion is the profile document version this build understands.
	// Unknown versions fail closed until the profile is reconciled.
	SchemaVersion = 1

	// maxInstructionsBytes bounds the pinned upstream server instructions.
	maxInstructionsBytes = 16 * 1024

	// maxStateReference bounds one state or credential reference name.
	maxStateReference = 64
)

// Profile is one validated Tama Link profile. One profile maps to exactly
// one upstream endpoint, one isolated SQLite database, and one credential
// namespace. Tool arguments must never select or override the endpoint.
type Profile struct {
	// Version is the profile document version.
	Version int `json:"version"`
	// Name must match the profile name resolved from the file path.
	Name Name `json:"name"`
	// Origin is the Tama protected-resource origin.
	Origin string `json:"origin"`
	// Endpoint is the single upstream MCP endpoint.
	Endpoint string `json:"endpoint"`
	// Issuer is the expected authorization-server issuer.
	Issuer string `json:"issuer"`
	// Instructions is the pinned bounded copy of the upstream server
	// instructions, verified against live initialization when connected.
	Instructions string `json:"instructions"`
	// Bounds are the protocol versions the profile is compatible with.
	Bounds Bounds `json:"bounds"`
	// State names the profile-isolated state database and credential
	// namespace.
	State StateRefs `json:"state"`
	// Limits pins the profile bounds. When unset, the version 1 defaults
	// apply; otherwise every field must be set.
	Limits *limits.Limits `json:"limits,omitempty"`
	// Operations is the pinned approved-operation catalog.
	Operations []catalog.Descriptor `json:"operations"`
}

// Bounds holds the protocol versions the profile is compatible with,
// written as MCP version dates.
type Bounds struct {
	// ProtocolMin is the oldest compatible protocol version.
	ProtocolMin string `json:"protocol_min"`
	// ProtocolMax is the newest compatible protocol version.
	ProtocolMax string `json:"protocol_max"`
}

// Valid reports whether both protocol bounds are version dates and
// ProtocolMin is not newer than ProtocolMax.
func (b Bounds) Valid() bool {
	lo, okLo := parseProtocolVersion(b.ProtocolMin)
	hi, okHi := parseProtocolVersion(b.ProtocolMax)
	return okLo && okHi && !lo.After(hi)
}

func parseProtocolVersion(value string) (time.Time, bool) {
	parsed, err := time.Parse("2006-01-02", value)
	return parsed, err == nil
}

// StateRefs names the profile-isolated state database and credential
// namespace inside the platform state and credential stores.
type StateRefs struct {
	// Database is the state-file reference for this profile.
	Database string `json:"database"`
	// Credentials is the credential-store namespace for this profile.
	Credentials string `json:"credentials"`
}

// Catalog returns the pinned approved-operation catalog.
func (p *Profile) Catalog() catalog.Catalog {
	return catalog.Catalog{Operations: p.Operations}
}

// EffectiveLimits returns the profile limits, or the version 1 defaults when
// the profile does not pin them.
func (p *Profile) EffectiveLimits() limits.Limits {
	if p.Limits == nil {
		return limits.Default()
	}
	return *p.Limits
}

// Validate reports whether the profile is internally consistent for the
// expected profile name.
func (p *Profile) Validate(expected Name) error {
	if p.Version != SchemaVersion {
		return fmt.Errorf("unsupported profile version %d, want %d", p.Version, SchemaVersion)
	}
	if p.Name != expected {
		return fmt.Errorf("profile name %q does not match %q", p.Name, expected)
	}
	origin, err := checkHTTPS("origin", p.Origin, false)
	if err != nil {
		return err
	}
	endpoint, err := checkHTTPS("endpoint", p.Endpoint, true)
	if err != nil {
		return err
	}
	if endpoint.Host != origin.Host {
		return fmt.Errorf("endpoint host %q does not match origin %q", endpoint.Host, origin.Host)
	}
	if _, err := checkHTTPS("issuer", p.Issuer, true); err != nil {
		return err
	}
	if len(p.Instructions) > maxInstructionsBytes {
		return fmt.Errorf("instructions exceed %d bytes", maxInstructionsBytes)
	}
	if !p.Bounds.Valid() {
		return fmt.Errorf("invalid protocol bounds %q to %q", p.Bounds.ProtocolMin, p.Bounds.ProtocolMax)
	}
	if err := p.State.Valid(); err != nil {
		return err
	}
	if err := p.EffectiveLimits().Validate(); err != nil {
		return err
	}
	if len(p.Operations) == 0 {
		return fmt.Errorf("profile has no operations")
	}
	seen := make(map[string]bool, len(p.Operations))
	for _, op := range p.Operations {
		if err := op.Validate(); err != nil {
			return err
		}
		if seen[op.Name] {
			return fmt.Errorf("duplicate operation %q", op.Name)
		}
		seen[op.Name] = true
	}
	return nil
}

// Valid reports whether both references are safe opaque names.
func (s StateRefs) Valid() error {
	if err := checkReference("database", s.Database); err != nil {
		return err
	}
	return checkReference("credentials", s.Credentials)
}

func checkReference(field, ref string) error {
	if ref == "" || ref == "." || ref == ".." {
		return fmt.Errorf("%s reference must be a non-empty name", field)
	}
	if len(ref) > maxStateReference {
		return fmt.Errorf("%s reference exceeds %d bytes", field, maxStateReference)
	}
	if strings.ContainsAny(ref, "/\\") || strings.Contains(ref, "..") {
		return fmt.Errorf("%s reference %q is not a safe name", field, ref)
	}
	return nil
}

// checkHTTPS parses value and requires an https URL without credentials,
// query, or fragment. When allowPath is false, the path must also be empty.
func checkHTTPS(field, value string, allowPath bool) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("%s %q is not a URL: %v", field, value, err)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("%s %q must use https", field, value)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%s %q must not contain credentials", field, value)
	}
	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("%s %q has no host", field, value)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("%s %q must not contain a query or fragment", field, value)
	}
	if !allowPath && parsed.Path != "" {
		return nil, fmt.Errorf("%s %q must be an origin without a path", field, value)
	}
	return parsed, nil
}
