package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ParamHeader is one schema-declared tools/call argument header. It is
// derived from the digested input schema, not stored beside it, so a change
// to the annotation changes the descriptor digest.
type ParamHeader struct {
	// Name is the x-mcp-header token, such as "Region".
	Name string
	// Path is the statically reachable property path, including the leaf.
	Path []string
	// Type is string, integer, or boolean.
	Type string
}

// headerToken is the HTTP field-name token grammar used by x-mcp-header.
var headerToken = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+$`)

// CheckInputSchema validates an upstream input schema, including its
// parameter-header annotations, then applies the ordinary vocabulary rules.
func CheckInputSchema(schema json.RawMessage) error {
	if _, err := ParamHeaders(schema); err != nil {
		return err
	}
	stripped, err := stripKeyword(schema, "x-mcp-header")
	if err != nil {
		return err
	}
	return CheckSchemaVocabulary(stripped)
}

// ParamHeaders extracts the reviewed parameter-header mappings from one
// input schema. Annotations are accepted only on properties reached through
// the properties keyword. Any other x-mcp-header is rejected.
func ParamHeaders(schema json.RawMessage) ([]ParamHeader, error) {
	members, err := objectMembers(schema)
	if err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	var found []ParamHeader
	if err := scanHeaders(members, nil, false, true, &found); err != nil {
		return nil, err
	}
	if err := uniqueHeaderNames(found); err != nil {
		return nil, err
	}
	slices.SortFunc(found, func(a, b ParamHeader) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return found, nil
}

func scanHeaders(members map[string]json.RawMessage, path []string, annotationAllowed, propertiesAllowed bool, found *[]ParamHeader) error {
	if raw, ok := members["x-mcp-header"]; ok {
		if !annotationAllowed {
			return fmt.Errorf("x-mcp-header at %s is not statically reachable", headerLocation(path))
		}
		header, err := headerDescriptor(members, path, raw)
		if err != nil {
			return err
		}
		*found = append(*found, header)
	}
	if err := scanProperties(members, path, propertiesAllowed, found); err != nil {
		return err
	}
	return scanUnreachable(members, path, found)
}

func scanProperties(members map[string]json.RawMessage, path []string, allowed bool, found *[]ParamHeader) error {
	raw, ok := members["properties"]
	if !ok {
		return nil
	}
	props, err := objectMembers(raw)
	if err != nil {
		return fmt.Errorf("properties at %s: %w", headerLocation(path), err)
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		child, err := objectMembers(props[name])
		if err != nil {
			return fmt.Errorf("property %s: %w", name, err)
		}
		childPath := append(append([]string(nil), path...), name)
		if err := scanHeaders(child, childPath, allowed, allowed, found); err != nil {
			return err
		}
	}
	return nil
}

func scanUnreachable(members map[string]json.RawMessage, path []string, found *[]ParamHeader) error {
	for _, key := range []string{"items", "additionalProperties"} {
		raw, ok := members[key]
		if !ok || !jsonObject(raw) {
			continue
		}
		child, err := objectMembers(raw)
		if err != nil {
			return err
		}
		if err := scanHeaders(child, path, false, false, found); err != nil {
			return err
		}
	}
	return nil
}

func headerDescriptor(members map[string]json.RawMessage, path []string, raw json.RawMessage) (ParamHeader, error) {
	var name string
	if err := json.Unmarshal(raw, &name); err != nil || name == "" {
		return ParamHeader{}, fmt.Errorf("x-mcp-header at %s must be a non-empty string", headerLocation(path))
	}
	if !headerToken.MatchString(name) {
		return ParamHeader{}, fmt.Errorf("x-mcp-header %q must be a valid HTTP field-name token", name)
	}
	var schemaType string
	if rawType, ok := members["type"]; ok {
		_ = json.Unmarshal(rawType, &schemaType)
	}
	switch schemaType {
	case "string", "integer", "boolean":
	default:
		return ParamHeader{}, fmt.Errorf("x-mcp-header %q must annotate a string, integer, or boolean property", name)
	}
	return ParamHeader{Name: name, Path: append([]string(nil), path...), Type: schemaType}, nil
}

func uniqueHeaderNames(headers []ParamHeader) error {
	seen := make(map[string]string, len(headers))
	for _, header := range headers {
		key := strings.ToLower(header.Name)
		if previous, ok := seen[key]; ok {
			return fmt.Errorf("duplicate x-mcp-header name %q", previous)
		}
		seen[key] = header.Name
	}
	return nil
}

func headerLocation(path []string) string {
	if len(path) == 0 {
		return "the schema root"
	}
	return strings.Join(path, ".")
}

func objectMembers(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if !jsonObject(raw) {
		return nil, fmt.Errorf("expected a JSON object")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	return members, nil
}

func jsonObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func stripKeyword(raw json.RawMessage, key string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw, nil
	}
	switch trimmed[0] {
	case '{':
		members, err := objectMembers(raw)
		if err != nil {
			return nil, err
		}
		delete(members, key)
		for name, child := range members {
			stripped, err := stripKeyword(child, key)
			if err != nil {
				return nil, err
			}
			members[name] = stripped
		}
		return json.Marshal(members)
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		for i, child := range items {
			stripped, err := stripKeyword(child, key)
			if err != nil {
				return nil, err
			}
			items[i] = stripped
		}
		return json.Marshal(items)
	default:
		return append(json.RawMessage(nil), raw...), nil
	}
}
