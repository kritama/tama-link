package profile

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/jsonvalue"
	"github.com/kritama/tama-link/internal/limits"
)

// digestForm is the complete non-secret profile configuration except for the
// digest itself. Fixed field order plus canonical JSON makes the digest stable
// across whitespace and object-key ordering differences.
type digestForm struct {
	Version      int                  `json:"version"`
	Name         Name                 `json:"name"`
	Origin       string               `json:"origin"`
	Endpoint     string               `json:"endpoint"`
	Issuer       string               `json:"issuer"`
	Instructions string               `json:"instructions"`
	Bounds       Bounds               `json:"bounds"`
	State        StateRefs            `json:"state"`
	Limits       *limits.Limits       `json:"limits,omitempty"`
	Operations   []catalog.Descriptor `json:"operations"`
	// Scopes participates in the digest from version 2: the canonical scope
	// set is profile identity, so a scope change without reconciliation
	// fails the pinned digest. Version 1 profiles carry an empty set and
	// their digests are unchanged.
	Scopes []string `json:"scopes,omitempty"`
}

// ComputeDigest computes the canonical digest for the complete profile
// configuration, excluding Digest. The scope set is canonicalized before
// encoding, so the digest never depends on the order a file happened to
// store the set in.
func (p Profile) ComputeDigest() (string, error) {
	scopes, err := CanonicalScopes(p.Scopes)
	if err != nil {
		return "", fmt.Errorf("digest scopes: %w", err)
	}
	encoded, err := json.Marshal(digestForm{
		Version:      p.Version,
		Name:         p.Name,
		Origin:       p.Origin,
		Endpoint:     p.Endpoint,
		Issuer:       p.Issuer,
		Instructions: p.Instructions,
		Bounds:       p.Bounds,
		State:        p.State,
		Limits:       p.Limits,
		Operations:   p.Operations,
		Scopes:       scopes,
	})
	if err != nil {
		return "", fmt.Errorf("encode profile for digest: %w", err)
	}
	canonical, err := jsonvalue.Canonical(encoded)
	if err != nil {
		return "", fmt.Errorf("canonicalize profile for digest: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return fmt.Sprintf("sha256:%x", sum), nil
}

// CheckDigest verifies the pinned digest over the complete profile.
func (p Profile) CheckDigest() error {
	computed, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	if computed != p.Digest {
		return fmt.Errorf("profile digest mismatch: pinned %s, computed %s", p.Digest, computed)
	}
	return nil
}
