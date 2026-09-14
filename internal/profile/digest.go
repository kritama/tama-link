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
}

// ComputeDigest computes the canonical digest for the complete profile
// configuration, excluding Digest.
func (p Profile) ComputeDigest() (string, error) {
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
