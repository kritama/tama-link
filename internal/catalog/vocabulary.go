package catalog

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
)

// enforcedKeywords is the complete set of assertion keywords the runtime
// validator enforces. annotationKeywords are carried but never asserted.
// Every other JSON Schema keyword would be silently unenforced, so
// CheckSchemaVocabulary rejects it at profile load instead (see there).
var enforcedKeywords = map[string]bool{
	"type":                 true,
	"properties":           true,
	"required":             true,
	"additionalProperties": true,
	"items":                true,
	"enum":                 true,
	"const":                true,
	"minimum":              true,
	"maximum":              true,
	"minLength":            true,
	"maxLength":            true,
	"minItems":             true,
	"maxItems":             true,
	"pattern":              true,
}

var annotationKeywords = map[string]bool{
	"title":       true,
	"description": true,
	"examples":    true,
	"default":     true,
	"$schema":     true,
	"$comment":    true,
}

// allowedTypes is the closed set of type names the dialect understands.
var allowedTypes = map[string]bool{
	"object":  true,
	"array":   true,
	"string":  true,
	"boolean": true,
	"null":    true,
	"number":  true,
	"integer": true,
}

// maxCountBound caps integer count constraints (minLength, maxLength,
// minItems, maxItems) so the runtime can hold them in a machine int and no
// pinned schema can encode an absurd or overflowing bound.
const maxCountBound = 1_000_000

// CheckSchemaVocabulary validates one pinned schema against the reviewed
// assertion dialect. It rejects any keyword outside the enforced and
// annotation vocabularies, and it validates the JSON type and legal value of
// every supported keyword, recursively through properties, items, and
// schema-form additionalProperties. null members are rejected rather than
// interpreted as absent, so a malformed schema can never disable an intended
// restriction; negative or empty bounds, duplicate or empty names,
// empty enums, and patterns that do not compile are rejected the same way.
//
// The pattern dialect is Go's RE2 syntax, a strict subset of the
// ECMAScript-compatible regular expressions JSON Schema normally assumes:
// no look-around, backreferences, or capture groups. Patterns must compile
// at profile load, so a profile can never pin a pattern the runtime cannot
// enforce.
//
// Called once per descriptor schema during profile validation.
func CheckSchemaVocabulary(schema json.RawMessage) error {
	members, err := schemaObject(schema, "$")
	if err != nil {
		return err
	}
	return checkNode(members, "$")
}

// checkNode validates one schema object node: keyword membership, the
// meta-shape of every present keyword, and the recursive children.
func checkNode(members map[string]json.RawMessage, path string) error {
	for name := range members {
		if !enforcedKeywords[name] && !annotationKeywords[name] {
			return fmt.Errorf("%s uses the unsupported schema keyword %q", path, name)
		}
	}
	if err := checkKeywordValues(members, path); err != nil {
		return err
	}
	if props, ok := members["properties"]; ok {
		children, err := schemaObject(props, path+".properties")
		if err != nil {
			return err
		}
		for name, child := range children {
			if name == "" {
				return fmt.Errorf("%s.properties has an empty property name", path)
			}
			childMembers, err := schemaObject(child, path+".properties."+name)
			if err != nil {
				return err
			}
			if err := checkNode(childMembers, path+".properties."+name); err != nil {
				return err
			}
		}
	}
	if items, ok := members["items"]; ok {
		itemMembers, err := schemaObject(items, path+".items")
		if err != nil {
			return err
		}
		if err := checkNode(itemMembers, path+".items"); err != nil {
			return err
		}
	}
	if addl, ok := members["additionalProperties"]; ok {
		trimmed := trimJSON(addl)
		switch {
		case string(trimmed) == "true" || string(trimmed) == "false":
		case bytesStartsWith(trimmed, "{"):
			addlMembers, err := schemaObject(addl, path+".additionalProperties")
			if err != nil {
				return err
			}
			if err := checkNode(addlMembers, path+".additionalProperties"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s.additionalProperties must be a boolean or a schema object, not %s", path, string(trimmed))
		}
	}
	return nil
}

// checkKeywordValues validates the JSON type and legal value of every
// supported keyword present at one node.
func checkKeywordValues(members map[string]json.RawMessage, path string) error {
	if raw, ok := members["type"]; ok {
		if err := checkTypeValue(raw, path); err != nil {
			return err
		}
	}
	if raw, ok := members["required"]; ok {
		names, err := requireUniqueStrings(raw, path, "required")
		if err != nil {
			return err
		}
		if props, ok := members["properties"]; ok {
			declared, err := schemaObject(props, path+".properties")
			if err != nil {
				return err
			}
			for _, name := range names {
				if _, ok := declared[name]; !ok {
					return fmt.Errorf("%s.required names %q, which is not a declared property", path, name)
				}
			}
		}
	}
	if raw, ok := members["enum"]; ok {
		values, err := requireArray(raw, path, "enum")
		if err != nil {
			return err
		}
		if len(values) == 0 {
			return fmt.Errorf("%s.enum must not be empty", path)
		}
	}
	for _, name := range []string{"minLength", "maxLength", "minItems", "maxItems"} {
		if raw, ok := members[name]; ok {
			if err := requireCount(raw, path, name); err != nil {
				return err
			}
		}
	}
	for _, name := range []string{"minimum", "maximum"} {
		if raw, ok := members[name]; ok {
			if err := requireNumberBound(raw, path, name); err != nil {
				return err
			}
		}
	}
	if raw, ok := members["pattern"]; ok {
		text, err := requireString(raw, path, "pattern")
		if err != nil {
			return err
		}
		if text == "" {
			return fmt.Errorf("%s.pattern must not be empty", path)
		}
		if _, err := regexp.Compile(text); err != nil {
			return fmt.Errorf("%s.pattern does not compile: %v", path, err)
		}
	}
	for _, name := range []string{"title", "description", "$comment", "$schema"} {
		if raw, ok := members[name]; ok {
			if _, err := requireString(raw, path, name); err != nil {
				return err
			}
		}
	}
	if raw, ok := members["examples"]; ok {
		if _, err := requireArray(raw, path, "examples"); err != nil {
			return err
		}
	}
	if err := checkPair(members, path, "minLength", "maxLength"); err != nil {
		return err
	}
	if err := checkPair(members, path, "minItems", "maxItems"); err != nil {
		return err
	}
	if err := checkPair(members, path, "minimum", "maximum"); err != nil {
		return err
	}
	return nil
}

// checkPair rejects an inverted low/high bound pair.
func checkPair(members map[string]json.RawMessage, path, low, high string) error {
	lowRaw, ok := members[low]
	if !ok {
		return nil
	}
	highRaw, ok := members[high]
	if !ok {
		return nil
	}
	if compareDecimal(lowRaw, highRaw) > 0 {
		return fmt.Errorf("%s.%s exceeds %s", path, low, high)
	}
	return nil
}

func checkTypeValue(raw json.RawMessage, path string) error {
	trimmed := trimJSON(raw)
	var names []string
	if bytesStartsWith(trimmed, "[") {
		items, err := requireArray(raw, path, "type")
		if err != nil {
			return err
		}
		if len(items) == 0 {
			return fmt.Errorf("%s.type must not be empty", path)
		}
		for _, item := range items {
			text, err := requireString(item, path, "type")
			if err != nil {
				return err
			}
			names = append(names, text)
		}
	} else {
		text, err := requireString(raw, path, "type")
		if err != nil {
			return err
		}
		names = []string{text}
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !allowedTypes[name] {
			return fmt.Errorf("%s.type uses the unsupported type %q", path, name)
		}
		if seen[name] {
			return fmt.Errorf("%s.type repeats %q", path, name)
		}
		seen[name] = true
	}
	return nil
}

func isNullRaw(raw json.RawMessage) bool {
	return string(trimJSON(raw)) == "null"
}

// schemaObject unmarshals one schema node, rejecting null explicitly:
// encoding/json silently turns null into an empty map, which would read as
// "no constraints" instead of failing closed.
func schemaObject(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	if isNullRaw(raw) {
		return nil, fmt.Errorf("%s must be a schema object, not null", path)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, fmt.Errorf("%s is not a schema object", path)
	}
	return members, nil
}

func requireString(raw json.RawMessage, path, what string) (string, error) {
	if isNullRaw(raw) {
		return "", fmt.Errorf("%s.%s must be a string, not null", path, what)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return "", fmt.Errorf("%s.%s must be a string", path, what)
	}
	return text, nil
}

func requireArray(raw json.RawMessage, path, what string) ([]json.RawMessage, error) {
	if isNullRaw(raw) {
		return nil, fmt.Errorf("%s.%s must be an array, not null", path, what)
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("%s.%s must be an array", path, what)
	}
	return values, nil
}

func requireUniqueStrings(raw json.RawMessage, path, what string) ([]string, error) {
	values, err := requireArray(raw, path, what)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		text, err := requireString(value, path, what)
		if err != nil {
			return nil, err
		}
		if text == "" {
			return nil, fmt.Errorf("%s.%s has an empty name", path, what)
		}
		if seen[text] {
			return nil, fmt.Errorf("%s.%s repeats %q", path, what, text)
		}
		seen[text] = true
		names = append(names, text)
	}
	return names, nil
}

// requireCount accepts a JSON number that is an integer within the dialect
// count bounds: non-negative and at most maxCountBound.
func requireCount(raw json.RawMessage, path, what string) error {
	if isNullRaw(raw) {
		return fmt.Errorf("%s.%s must be an integer, not null", path, what)
	}
	trimmed := trimJSON(raw)
	if !isJSONNumber(trimmed) {
		return fmt.Errorf("%s.%s must be a JSON number", path, what)
	}
	neg, _, fracPart := normalizeNumber(trimmed)
	if neg || len(fracPart) > 0 {
		return fmt.Errorf("%s.%s must be a non-negative integer", path, what)
	}
	if compareDecimal(trimmed, []byte(strconv.Itoa(maxCountBound))) > 0 {
		return fmt.Errorf("%s.%s exceeds the bound of %d", path, what, maxCountBound)
	}
	return nil
}

// requireNumberBound accepts the complete JSON number grammar, including
// exponent form, and nothing else.
func requireNumberBound(raw json.RawMessage, path, what string) error {
	if isNullRaw(raw) {
		return fmt.Errorf("%s.%s must be a number, not null", path, what)
	}
	if !isJSONNumber(trimJSON(raw)) {
		return fmt.Errorf("%s.%s must be a JSON number", path, what)
	}
	return nil
}
