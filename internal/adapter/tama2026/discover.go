package tama2026

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/upstream"
)

// classify maps an upstream transport error onto the adapter boundary.
// Protocol failures are adapter boundary errors; transport, HTTP, and
// oversize failures pass through for the contract boundary to classify.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if upstream.IsAuth(err) {
		return fmt.Errorf("%w: upstream rejected the credential", ErrAuthenticationRequired)
	}
	if upstream.IsProtocol(err) {
		return fmt.Errorf("%w: %w", ErrProtocolMismatch, err)
	}
	return err
}

// verifyDiscovery enforces the pinned protocol and server capability
// contract before any catalog work begins.
func (a *Adapter) verifyDiscovery(disc *upstream.DiscoverResult) error {
	if !a.profile.Bounds.Contains(protocolVersion) {
		return fmt.Errorf("%w: protocol %s is outside the profile bounds %s to %s",
			ErrProtocolMismatch, protocolVersion, a.profile.Bounds.ProtocolMin, a.profile.Bounds.ProtocolMax)
	}
	if !slices.Contains(disc.SupportedVersions, protocolVersion) {
		return fmt.Errorf("%w: server does not support %s", ErrProtocolMismatch, protocolVersion)
	}
	if _, err := decodeCapabilities(disc.Capabilities); err != nil {
		return fmt.Errorf("%w: %w", ErrProtocolMismatch, err)
	}
	if a.profile.Instructions != disc.Instructions {
		return fmt.Errorf("%w: live instructions do not match the pinned profile", ErrCatalogMismatch)
	}
	return nil
}

// capabilityView projects the raw server capabilities into the fields the
// adapter verifies. Unknown fields survive in the raw snapshot.
type capabilityView struct {
	Tools      json.RawMessage
	Extensions map[string]json.RawMessage
}

// decodeCapabilities enforces the capability object shapes: capabilities
// must be a JSON object, tools must be declared as a JSON object, and every
// declared extension value must be a JSON object. A null or scalar
// declaration is malformed, not an empty object.
func decodeCapabilities(raw json.RawMessage) (*capabilityView, error) {
	if !upstream.IsJSONObject(raw) {
		return nil, fmt.Errorf("capabilities is not a JSON object")
	}
	var view capabilityView
	if err := json.Unmarshal(raw, &view); err != nil {
		return nil, fmt.Errorf("decode server capabilities")
	}
	if len(view.Tools) == 0 || !upstream.IsJSONObject(view.Tools) {
		return nil, fmt.Errorf("server declares no tools capability object")
	}
	for _, ext := range view.Extensions {
		if !upstream.IsJSONObject(ext) {
			return nil, fmt.Errorf("capability extension value is not a JSON object")
		}
	}
	return &view, nil
}

// verifyCatalog intersects the live tools with the pinned descriptors and
// fails closed on any missing or drifted approved operation.
func (a *Adapter) verifyCatalog(disc *upstream.DiscoverResult, live []*upstream.LiveTool) (catalog.Catalog, error) {
	liveByName := make(map[string]*upstream.LiveTool, len(live))
	for _, tool := range live {
		if _, exists := liveByName[tool.Name]; exists {
			return catalog.Catalog{}, fmt.Errorf("%w: live catalog repeats tool %q", ErrProtocolMismatch, tool.Name)
		}
		liveByName[tool.Name] = tool
	}

	pinned := a.profile.Catalog().Operations
	effective := catalog.Catalog{Operations: make([]catalog.Descriptor, 0, len(pinned))}
	for _, d := range pinned {
		tool, ok := liveByName[d.Name]
		if !ok {
			return catalog.Catalog{}, fmt.Errorf("%w: approved tool %q is not live", ErrCatalogMismatch, d.Name)
		}
		if d.Strategy == catalog.StrategyUpstreamTask && !disc.HasTaskExtension() {
			return catalog.Catalog{}, fmt.Errorf("%w: tool %q requires the tasks extension", ErrCatalogMismatch, d.Name)
		}
		if drifted := d.Drift(normalizeLive(tool, d)); len(drifted) > 0 {
			return catalog.Catalog{}, fmt.Errorf("%w: tool %q drifted on %s", ErrCatalogMismatch, d.Name, joinDrift(drifted))
		}
		effective.Operations = append(effective.Operations, d)
	}
	return effective, nil
}

// normalizeLive projects one raw live tool into the catalog comparison
// form. The task support is adopted from the pinned descriptor: the live
// wire carries no per-tool task field in 2026-07-28, and the effective
// task behavior is enforced by the discovered Tasks extension plus the
// per-request capability and the observed resultType, never inferred from a
// missing field.
func normalizeLive(tool *upstream.LiveTool, pinned catalog.Descriptor) catalog.LiveTool {
	return catalog.LiveTool{
		Name:         tool.Name,
		InputSchema:  tool.InputSchema,
		OutputSchema: tool.OutputSchema,
		Annotations:  annotationsMap(tool.Annotations),
		TaskSupport:  pinned.TaskSupport,
	}
}

// annotationsMap decodes the raw annotations object, keeping nil for an
// absent declaration. Numbers decode as json.Number so the pinned literal
// survives the comparison: float64 coercion would re-encode an unchanged
// 1.0 as 1 (false drift) and could collapse distinct integers above 2^53
// into one value (missed drift), diverging from the profile loader's
// UseNumber decode.
func annotationsMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return map[string]any{"__unparseable__": true}
	}
	return out
}

// joinDrift renders drifted fields deterministically.
func joinDrift(fields []string) string {
	sorted := append([]string(nil), fields...)
	slices.Sort(sorted)
	out := ""
	for i, f := range sorted {
		if i > 0 {
			out += ", "
		}
		out += f
	}
	return out
}
