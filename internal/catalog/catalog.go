// Package catalog defines the pinned operation descriptor model, the
// profile operation allowlist, drift verification against live upstream
// tools, and the bounded schema projection advertised downstream.
//
// The effective catalog is always the live catalog intersected with this
// allowlist: a live tool that is not pinned is never exposed, and a pinned
// operation with security-relevant drift fails closed.
package catalog

import (
	"fmt"
	"slices"
)

// Catalog is the pinned allowlist of approved upstream operations.
type Catalog struct {
	Operations []Descriptor `json:"operations"`
}

// Validate reports whether the catalog is structurally well-formed:
// operation names are unique and every descriptor is valid.
func (c Catalog) Validate() error {
	seen := make(map[string]bool, len(c.Operations))
	for _, d := range c.Operations {
		if seen[d.Name] {
			return fmt.Errorf("duplicate operation %q", d.Name)
		}
		seen[d.Name] = true
		if err := d.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Find returns the pinned descriptor for name.
func (c Catalog) Find(name string) (Descriptor, bool) {
	for _, d := range c.Operations {
		if d.Name == name {
			return d, true
		}
	}
	return Descriptor{}, false
}

// Names returns the approved operation names sorted for deterministic
// schema projection.
func (c Catalog) Names() []string {
	names := make([]string, 0, len(c.Operations))
	for _, d := range c.Operations {
		names = append(names, d.Name)
	}
	slices.Sort(names)
	return names
}
