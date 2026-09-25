package profile

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/limits"
)

var databaseReferencePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

const (
	// SchemaVersion is the newest profile document version this build
	// understands: version 2 adds the required non-empty OAuth scopes array.
	SchemaVersion = 2

	// LegacySchemaVersion is the version 1 document delivered through
	// Phase 2. It remains loadable for the non-interactive runtime and
	// fails interactive login with a migration error instead of receiving
	// an implicit scope set.
	LegacySchemaVersion = 1

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
	// Scopes is the canonical, sorted OAuth scope set the profile requests.
	// Required and non-empty in version 2; absent in version 1. The
	// canonical set participates in the profile digest.
	Scopes []string `json:"scopes,omitempty"`
	// Digest reconciles the complete non-secret profile configuration. It is
	// required when any profile limit exceeds the version 1 default.
	Digest string `json:"digest,omitempty"`
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

// Contains reports whether version, an MCP version date, falls inclusively
// within the bounds. Version dates compare lexicographically, so the
// comparison is a plain string comparison.
func (b Bounds) Contains(version string) bool {
	return version >= b.ProtocolMin && version <= b.ProtocolMax
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
	if p.Version != SchemaVersion && p.Version != LegacySchemaVersion {
		return fmt.Errorf("unsupported profile version %d, want %d or %d", p.Version, SchemaVersion, LegacySchemaVersion)
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
	effectiveLimits := p.EffectiveLimits()
	if err := effectiveLimits.Validate(); err != nil {
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
	if _, err := p.Kind(); err != nil {
		return err
	}
	canonical, err := validateScopes(p.Version, p.Scopes)
	if err != nil {
		return err
	}
	p.Scopes = canonical
	if p.Digest != "" {
		if err := p.CheckDigest(); err != nil {
			return err
		}
	}
	if raisesDefaultLimits(effectiveLimits) && p.Digest == "" {
		return fmt.Errorf("profile digest is required when limits exceed defaults")
	}
	return nil
}

func raisesDefaultLimits(got limits.Limits) bool {
	defaults := limits.Default()
	return got.ArgumentsBytes > defaults.ArgumentsBytes ||
		got.ArgumentDepth > defaults.ArgumentDepth ||
		got.ResponseBytes > defaults.ResponseBytes ||
		got.ResultBytes > defaults.ResultBytes ||
		got.EventBytes > defaults.EventBytes ||
		got.MaxEvents > defaults.MaxEvents ||
		got.EventsBytes > defaults.EventsBytes ||
		got.AwaitDefault > defaults.AwaitDefault ||
		got.AwaitMax > defaults.AwaitMax ||
		got.PayloadRetention > defaults.PayloadRetention ||
		got.TombstoneRetention > defaults.TombstoneRetention
}

// Valid reports whether both references are safe opaque names.
func (s StateRefs) Valid() error {
	if err := checkDatabaseReference(s.Database); err != nil {
		return err
	}
	return checkReference("credentials", s.Credentials)
}

func checkDatabaseReference(ref string) error {
	if err := checkReference("database", ref); err != nil {
		return err
	}
	if !databaseReferencePattern.MatchString(ref) || windowsReservedDatabaseReference(ref) {
		return fmt.Errorf("database reference %q is not a portable filename", ref)
	}
	return nil
}

func windowsReservedDatabaseReference(ref string) bool {
	switch ref {
	case "aux", "con", "nul", "prn":
		return true
	}
	return len(ref) == 4 && (strings.HasPrefix(ref, "com") || strings.HasPrefix(ref, "lpt")) &&
		ref[3] >= '1' && ref[3] <= '9'
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
