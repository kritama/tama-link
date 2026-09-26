package tama2026

import (
	"fmt"
	"slices"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/upstream"
)

// Template is one reviewed profile policy. It is release content, not a
// document downloaded from the address the user entered.
type Template struct {
	ID            string
	Kind          profile.Kind
	Label         string
	Instructions  string
	Bounds        profile.Bounds
	Scopes        []string
	Operations    []catalog.Descriptor
	RequiresTasks bool
}

// DefaultName is the portable profile name for kind when the user does not
// supply one.
func DefaultName(kind profile.Kind) string {
	if kind == profile.KindSystem {
		return "tama-system"
	}
	return "tama-app"
}

// Lookup returns the reviewed template for an app or system endpoint.
func Lookup(kind profile.Kind) (Template, error) {
	var tmpl Template
	switch kind {
	case profile.KindApp:
		tmpl = appTemplate()
	case profile.KindSystem:
		tmpl = systemTemplate()
	default:
		return Template{}, fmt.Errorf("unsupported profile type %q", kind)
	}
	scopes, err := profile.CanonicalScopes(tmpl.Scopes)
	if err != nil {
		return Template{}, fmt.Errorf("template %s scopes: %w", tmpl.ID, err)
	}
	tmpl.Scopes = scopes
	for i := range tmpl.Operations {
		digest, err := tmpl.Operations[i].ComputeDigest()
		if err != nil {
			return Template{}, fmt.Errorf("template %s descriptor %s: %w", tmpl.ID, tmpl.Operations[i].Name, err)
		}
		tmpl.Operations[i].Digest = digest
		if err := tmpl.Operations[i].Validate(); err != nil {
			return Template{}, fmt.Errorf("template %s descriptor %s: %w", tmpl.ID, tmpl.Operations[i].Name, err)
		}
	}
	if !tmpl.Bounds.Contains(protocolVersion) {
		return Template{}, fmt.Errorf("template %s does not cover protocol %s", tmpl.ID, protocolVersion)
	}
	return tmpl, nil
}

// Materialize verifies the live discovery against the reviewed template and
// returns the template descriptors. Extra live tools are ignored. Missing
// or drifted approved tools fail. Live schemas are never copied into the
// result.
func (t Template) Materialize(disc *upstream.DiscoverResult, live []*upstream.LiveTool) ([]catalog.Descriptor, error) {
	if disc == nil {
		return nil, fmt.Errorf("discover result is required")
	}
	if !slices.Contains(disc.SupportedVersions, protocolVersion) {
		return nil, fmt.Errorf("%w: server does not support %s", ErrProtocolMismatch, protocolVersion)
	}
	if _, err := decodeCapabilities(disc.Capabilities); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrProtocolMismatch, err)
	}
	if t.RequiresTasks && !disc.HasTaskExtension() {
		return nil, fmt.Errorf("%w: template requires the tasks extension", ErrCatalogMismatch)
	}
	if t.Instructions != disc.Instructions {
		return nil, fmt.Errorf("%w: live instructions do not match the reviewed template", ErrCatalogMismatch)
	}
	liveByName := make(map[string]*upstream.LiveTool, len(live))
	for _, tool := range live {
		if tool == nil || tool.Name == "" {
			return nil, fmt.Errorf("%w: live catalog contains an unnamed tool", ErrProtocolMismatch)
		}
		if _, exists := liveByName[tool.Name]; exists {
			return nil, fmt.Errorf("%w: live catalog repeats tool %q", ErrProtocolMismatch, tool.Name)
		}
		liveByName[tool.Name] = tool
	}
	out := make([]catalog.Descriptor, 0, len(t.Operations))
	for _, proto := range t.Operations {
		tool, ok := liveByName[proto.Name]
		if !ok {
			return nil, fmt.Errorf("%w: approved tool %q is not live", ErrCatalogMismatch, proto.Name)
		}
		if drifted := proto.Drift(normalizeLive(tool, proto)); len(drifted) > 0 {
			return nil, fmt.Errorf("%w: tool %q drifted on %s", ErrCatalogMismatch, proto.Name, joinDrift(drifted))
		}
		out = append(out, proto)
	}
	return out, nil
}
