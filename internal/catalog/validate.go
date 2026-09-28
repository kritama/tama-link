package catalog

import (
	"encoding/json"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/cockroachdb/apd/v3"
)

// ValidateAgainstSchema validates one JSON value against the pinned schema's
// enforced vocabulary: type, properties, required, additionalProperties
// (boolean or nested schema), items, enum, const, minimum, maximum,
// minLength, maxLength (Unicode code points), minItems, maxItems,
// pattern (Go RE2), and anyOf. Pinned schemas may only use this vocabulary with
// well-formed values: CheckSchemaVocabulary enforces both at profile load,
// so nothing unenforced or malformed can reach runtime. Numbers are compared
// through exact arbitrary-precision decimals — exponent form included — never
// through float64.
func ValidateAgainstSchema(schema, value json.RawMessage) error {
	var s schemaView
	if err := json.Unmarshal(schema, &s); err != nil {
		return fmt.Errorf("schema is not a JSON object: %w", err)
	}
	return s.validate(value, "$")
}

// schemaView is the reviewed keyword subset one schema node declares.
type schemaView struct {
	Type                 json.RawMessage            `json:"type"`
	Properties           map[string]json.RawMessage `json:"properties"`
	Required             []string                   `json:"required"`
	AdditionalProperties json.RawMessage            `json:"additionalProperties"`
	Items                json.RawMessage            `json:"items"`
	Enum                 []json.RawMessage          `json:"enum"`
	Const                json.RawMessage            `json:"const"`
	Minimum              *numberText                `json:"minimum"`
	Maximum              *numberText                `json:"maximum"`
	MinLength            *countValue                `json:"minLength"`
	MaxLength            *countValue                `json:"maxLength"`
	MinItems             *countValue                `json:"minItems"`
	MaxItems             *countValue                `json:"maxItems"`
	Pattern              string                     `json:"pattern"`
	AnyOf                []json.RawMessage          `json:"anyOf"`
}

func (s *schemaView) validate(value json.RawMessage, path string) error {
	trimmed := trimJSON(value)
	kind := valueKind(trimmed)
	if s.Type != nil {
		if err := s.checkType(trimmed, kind, path); err != nil {
			return err
		}
	}
	if len(s.AnyOf) > 0 {
		if err := s.validateAnyOf(trimmed, path); err != nil {
			return err
		}
	}
	switch kind {
	case jsonKindObject:
		if err := s.validateObject(trimmed, path); err != nil {
			return err
		}
	case jsonKindArray:
		if err := s.validateArray(trimmed, path); err != nil {
			return err
		}
	case jsonKindString:
		if err := s.validateString(trimmed, path); err != nil {
			return err
		}
	case jsonKindNumber:
		if err := s.validateNumber(trimmed, path); err != nil {
			return err
		}
	}
	if s.Const != nil {
		eq, err := jsonInstanceEqual(trimmed, s.Const)
		if err != nil {
			return fmt.Errorf("%s does not match the pinned const: %v", path, err)
		}
		if !eq {
			return fmt.Errorf("%s does not match the pinned const", path)
		}
	}
	if len(s.Enum) > 0 {
		for _, option := range s.Enum {
			eq, err := jsonInstanceEqual(trimmed, option)
			if err == nil && eq {
				return nil
			}
		}
		return fmt.Errorf("%s is not one of the pinned enum values", path)
	}
	return nil
}

func (s *schemaView) validateAnyOf(value []byte, path string) error {
	for i, branch := range s.AnyOf {
		var child schemaView
		if err := json.Unmarshal(branch, &child); err != nil {
			return fmt.Errorf("%s.anyOf[%d] is not a schema object", path, i)
		}
		if err := child.validate(value, fmt.Sprintf("%s.anyOf[%d]", path, i)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%s does not match any allowed schema", path)
}

func (s *schemaView) checkType(value []byte, kind jsonKind, path string) error {
	trimmed := trimJSON(s.Type)
	var types []string
	if bytesStartsWith(trimmed, "[") {
		var list []json.RawMessage
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return fmt.Errorf("pinned type is not a string or array: %w", err)
		}
		for _, item := range list {
			var text string
			if err := json.Unmarshal(item, &text); err != nil {
				return fmt.Errorf("pinned type list contains a non-string: %w", err)
			}
			types = append(types, text)
		}
	} else {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return fmt.Errorf("pinned type is not a string: %w", err)
		}
		types = []string{text}
	}
	for _, expected := range types {
		if s.typeMatches(expected, value, kind) {
			return nil
		}
	}
	return fmt.Errorf("%s has type %s, want one of %v", path, kind, types)
}

func (s *schemaView) typeMatches(expected string, value []byte, kind jsonKind) bool {
	switch expected {
	case "object", "array", "string", "boolean", "null", "number":
		return kind == jsonKind(expected)
	case "integer":
		return kind == jsonKindNumber && isIntegerLiteral(value)
	}
	return false
}

func (s *schemaView) validateObject(value json.RawMessage, path string) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(value, &members); err != nil {
		return fmt.Errorf("%s is not a JSON object: %w", path, err)
	}
	// required checks map-key presence only, per JSON Schema: a present
	// property whose value is an explicit JSON null satisfies the
	// requirement, and the property's own schema decides whether null is
	// permitted. No default annotation fills a missing property.
	for _, name := range s.Required {
		if _, ok := members[name]; !ok {
			return fmt.Errorf("%s is missing the required property %q", path, name)
		}
	}
	for name, prop := range s.Properties {
		child, ok := members[name]
		if !ok {
			continue
		}
		var childView schemaView
		if err := json.Unmarshal(prop, &childView); err != nil {
			return fmt.Errorf("%s.%s has a malformed property schema: %w", path, name, err)
		}
		if err := childView.validate(child, path+"."+name); err != nil {
			return err
		}
	}
	return s.validateAdditional(members, path)
}

// validateAdditional enforces the pinned additionalProperties constraint:
// false rejects unknown properties, true admits them, and a nested schema
// validates every unknown property.
func (s *schemaView) validateAdditional(members map[string]json.RawMessage, path string) error {
	if s.AdditionalProperties == nil {
		return nil
	}
	trimmed := trimJSON(s.AdditionalProperties)
	switch {
	case string(trimmed) == "false":
		known := make(map[string]bool, len(s.Properties))
		for name := range s.Properties {
			known[name] = true
		}
		for name := range members {
			if !known[name] {
				return fmt.Errorf("%s has the unapproved property %q", path, name)
			}
		}
	case string(trimmed) == "true":
		return nil
	case bytesStartsWith(trimmed, "{"):
		var childView schemaView
		if err := json.Unmarshal(s.AdditionalProperties, &childView); err != nil {
			return fmt.Errorf("%s.additionalProperties is a malformed schema: %w", path, err)
		}
		known := make(map[string]bool, len(s.Properties))
		for name := range s.Properties {
			known[name] = true
		}
		for name, child := range members {
			if known[name] {
				continue
			}
			if err := childView.validate(child, path+"."+name); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s.additionalProperties is not a boolean or a schema", path)
	}
	return nil
}

func (s *schemaView) validateArray(value json.RawMessage, path string) error {
	var items []json.RawMessage
	if err := json.Unmarshal(value, &items); err != nil {
		return fmt.Errorf("%s is not a JSON array: %w", path, err)
	}
	if s.MinItems != nil && len(items) < int(*s.MinItems) {
		return fmt.Errorf("%s has fewer than %d items", path, *s.MinItems)
	}
	if s.MaxItems != nil && len(items) > int(*s.MaxItems) {
		return fmt.Errorf("%s has more than %d items", path, *s.MaxItems)
	}
	if s.Items == nil {
		return nil
	}
	var itemView schemaView
	if err := json.Unmarshal(s.Items, &itemView); err != nil {
		return fmt.Errorf("%s has a malformed item schema: %w", path, err)
	}
	for i, item := range items {
		if err := itemView.validate(item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

func (s *schemaView) validateString(value json.RawMessage, path string) error {
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return fmt.Errorf("%s is not a JSON string: %w", path, err)
	}
	// JSON Schema string lengths count Unicode code points, not UTF-8 bytes.
	length := utf8.RuneCountInString(text)
	if s.MinLength != nil && length < int(*s.MinLength) {
		return fmt.Errorf("%s is shorter than %d characters", path, *s.MinLength)
	}
	if s.MaxLength != nil && length > int(*s.MaxLength) {
		return fmt.Errorf("%s is longer than %d characters", path, *s.MaxLength)
	}
	if s.Pattern != "" {
		re, err := regexp.Compile(s.Pattern)
		if err != nil {
			return fmt.Errorf("pinned pattern does not compile: %w", err)
		}
		if !re.MatchString(text) {
			return fmt.Errorf("%s does not match the pinned pattern", path)
		}
	}
	return nil
}

func (s *schemaView) validateNumber(value json.RawMessage, path string) error {
	if s.Minimum == nil && s.Maximum == nil {
		return nil
	}
	num, err := parseJSONNumber(value)
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	if s.Minimum != nil && num.Cmp(s.Minimum.dec) < 0 {
		return fmt.Errorf("%s is below the pinned minimum", path)
	}
	if s.Maximum != nil && num.Cmp(s.Maximum.dec) > 0 {
		return fmt.Errorf("%s is above the pinned maximum", path)
	}
	return nil
}

// numberText is a JSON Schema numeric bound in exact arbitrary-precision
// decimal form, so comparison never routes through float64 and never
// allocates in proportion to the exponent magnitude. It accepts the
// complete JSON number grammar, including exponent form, within the
// reviewed exponent range.
type numberText struct {
	dec *apd.Decimal
}

func (n *numberText) UnmarshalJSON(raw []byte) error {
	trimmed := trimJSON(raw)
	if !isJSONNumber(trimmed) {
		return fmt.Errorf("bound is not a JSON number")
	}
	dec, err := parseJSONNumber(trimmed)
	if err != nil {
		return err
	}
	n.dec = dec
	return nil
}

// jsonKind classifies a trimmed JSON literal.
type jsonKind string

const (
	jsonKindNull    jsonKind = "null"
	jsonKindObject  jsonKind = "object"
	jsonKindArray   jsonKind = "array"
	jsonKindString  jsonKind = "string"
	jsonKindBoolean jsonKind = "boolean"
	jsonKindNumber  jsonKind = "number"
)

func valueKind(trimmed []byte) jsonKind {
	switch {
	case string(trimmed) == "null":
		return jsonKindNull
	case bytesStartsWith(trimmed, "{"):
		return jsonKindObject
	case bytesStartsWith(trimmed, "["):
		return jsonKindArray
	case bytesStartsWith(trimmed, "\""):
		return jsonKindString
	case string(trimmed) == "true" || string(trimmed) == "false":
		return jsonKindBoolean
	case isJSONNumber(trimmed):
		return jsonKindNumber
	}
	return "unknown"
}

// isIntegerLiteral reports whether a number literal denotes an integer
// (1e2 and 15.0 are integers, 1e-1 and 1.5 are not), through the exact
// decimal model. A literal whose exponent is outside the reviewed range
// cannot be evaluated and therefore does not match.
func isIntegerLiteral(value []byte) bool {
	if !isJSONNumber(trimJSON(value)) {
		return false
	}
	dec, err := parseJSONNumber(value)
	if err != nil {
		return false
	}
	return isExactInteger(dec)
}

// isExactInteger reports whether one exact decimal denotes an integer value.
// Reducing trailing coefficient zeros avoids constructing 10^scale, so work
// depends on the supplied coefficient rather than the exponent magnitude.
func isExactInteger(dec *apd.Decimal) bool {
	if dec.Form != apd.Finite {
		return false
	}
	var reduced apd.Decimal
	reduced.Reduce(dec)
	return reduced.Exponent >= 0
}

func trimJSON(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func bytesStartsWith(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == prefix
}

// isJSONNumber reports whether trimmed is a complete JSON number literal:
// -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)? — including exponent form.
func isJSONNumber(trimmed []byte) bool {
	if len(trimmed) == 0 {
		return false
	}
	i := 0
	if trimmed[0] == '-' {
		i++
	}
	if i >= len(trimmed) {
		return false
	}
	if trimmed[i] == '0' {
		i++
	} else if trimmed[i] >= '1' && trimmed[i] <= '9' {
		i++
		for i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9' {
			i++
		}
	} else {
		return false
	}
	if i < len(trimmed) && trimmed[i] == '.' {
		i++
		if i >= len(trimmed) || trimmed[i] < '0' || trimmed[i] > '9' {
			return false
		}
		for i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9' {
			i++
		}
	}
	if i < len(trimmed) && (trimmed[i] == 'e' || trimmed[i] == 'E') {
		i++
		if i < len(trimmed) && (trimmed[i] == '+' || trimmed[i] == '-') {
			i++
		}
		if i >= len(trimmed) || trimmed[i] < '0' || trimmed[i] > '9' {
			return false
		}
		for i < len(trimmed) && trimmed[i] >= '0' && trimmed[i] <= '9' {
			i++
		}
	}
	return i == len(trimmed)
}
