package catalog

import (
	"encoding/json"
	"fmt"
	"regexp"
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

// CheckSchemaVocabulary walks one pinned schema and rejects any assertion
// keyword the runtime validator does not enforce, including keywords nested
// behind properties, items, and schema-form additionalProperties. A profile
// that pins oneOf, allOf, not, minProperties, uniqueItems, contains,
// exclusiveMinimum, dependentRequired, or another unsupported assertion fails
// closed at load; it can never be silently accepted with the assertion
// unenforced. Called once per descriptor schema during profile validation.
func CheckSchemaVocabulary(schema json.RawMessage) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(schema, &members); err != nil {
		return fmt.Errorf("schema is not a JSON object: %w", err)
	}
	return checkVocabulary(members, "$")
}

func checkVocabulary(members map[string]json.RawMessage, path string) error {
	for name := range members {
		if !enforcedKeywords[name] && !annotationKeywords[name] {
			return fmt.Errorf("%s uses the unsupported schema keyword %q", path, name)
		}
	}
	if props, ok := members["properties"]; ok {
		var children map[string]json.RawMessage
		if err := json.Unmarshal(props, &children); err != nil {
			return fmt.Errorf("%s.properties is not an object: %w", path, err)
		}
		for name, child := range children {
			var childMembers map[string]json.RawMessage
			if err := json.Unmarshal(child, &childMembers); err != nil {
				return fmt.Errorf("%s.properties.%s is not an object: %w", path, name, err)
			}
			if err := checkVocabulary(childMembers, path+".properties."+name); err != nil {
				return err
			}
		}
	}
	if items, ok := members["items"]; ok {
		var itemMembers map[string]json.RawMessage
		if err := json.Unmarshal(items, &itemMembers); err != nil {
			return fmt.Errorf("%s.items is not an object: %w", path, err)
		}
		if err := checkVocabulary(itemMembers, path+".items"); err != nil {
			return err
		}
	}
	if addl, ok := members["additionalProperties"]; ok && len(trimJSON(addl)) > 0 && trimJSON(addl)[0] == '{' {
		var addlMembers map[string]json.RawMessage
		if err := json.Unmarshal(addl, &addlMembers); err != nil {
			return fmt.Errorf("%s.additionalProperties is not a boolean or a schema: %w", path, err)
		}
		if err := checkVocabulary(addlMembers, path+".additionalProperties"); err != nil {
			return err
		}
	}
	return nil
}

// ValidateAgainstSchema validates one JSON value against the pinned schema's
// enforced vocabulary: type, properties, required, additionalProperties
// (boolean or nested schema), items, enum, const, minimum, maximum,
// minLength, maxLength, minItems, maxItems, and pattern. Pinned schemas may
// only use this vocabulary: CheckSchemaVocabulary rejects any other assertion
// keyword at profile load, so nothing unenforced can reach runtime. Numbers
// are compared through their exact decimal text, never through float64.
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
	MinLength            *int                       `json:"minLength"`
	MaxLength            *int                       `json:"maxLength"`
	MinItems             *int                       `json:"minItems"`
	MaxItems             *int                       `json:"maxItems"`
	Pattern              string                     `json:"pattern"`
}

func (s *schemaView) validate(value json.RawMessage, path string) error {
	trimmed := trimJSON(value)
	kind := valueKind(trimmed)
	if s.Type != nil {
		if err := s.checkType(trimmed, kind, path); err != nil {
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
	if s.Const != nil && !jsonValuesEqual(trimmed, s.Const) {
		return fmt.Errorf("%s does not match the pinned const", path)
	}
	if len(s.Enum) > 0 {
		for _, option := range s.Enum {
			if jsonValuesEqual(trimmed, option) {
				return nil
			}
		}
		return fmt.Errorf("%s is not one of the pinned enum values", path)
	}
	return nil
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
	if s.MinItems != nil && len(items) < *s.MinItems {
		return fmt.Errorf("%s has fewer than %d items", path, *s.MinItems)
	}
	if s.MaxItems != nil && len(items) > *s.MaxItems {
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
	if s.MinLength != nil && len(text) < *s.MinLength {
		return fmt.Errorf("%s is shorter than %d bytes", path, *s.MinLength)
	}
	if s.MaxLength != nil && len(text) > *s.MaxLength {
		return fmt.Errorf("%s is longer than %d bytes", path, *s.MaxLength)
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
	if s.Minimum != nil && compareDecimal(value, s.Minimum.literal) < 0 {
		return fmt.Errorf("%s is below the pinned minimum", path)
	}
	if s.Maximum != nil && compareDecimal(value, s.Maximum.literal) > 0 {
		return fmt.Errorf("%s is above the pinned maximum", path)
	}
	return nil
}

// numberText is a JSON Schema numeric bound with its exact literal text.
// It accepts a JSON number (the standard form) and records the digits
// verbatim so comparison never routes through float64.
type numberText struct {
	literal []byte
}

func (n *numberText) UnmarshalJSON(raw []byte) error {
	trimmed := trimJSON(raw)
	if !isJSONNumber(trimmed) {
		return fmt.Errorf("bound is not a JSON number")
	}
	n.literal = append([]byte(nil), trimmed...)
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

// isIntegerLiteral reports whether a number literal denotes an integer: no
// fractional digits remain after stripping trailing zeros.
func isIntegerLiteral(value []byte) bool {
	text := trimJSON(value)
	if bytesStartsWith(text, ".") {
		return false
	}
	dot := -1
	for i := 0; i < len(text); i++ {
		if text[i] == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		return true
	}
	for i := dot + 1; i < len(text); i++ {
		if text[i] != '0' {
			return false
		}
	}
	return true
}

// splitDecimalParts splits decimal text into signed integer-part bytes and
// fractional digits (trailing zeros dropped).
func splitDecimalParts(text []byte) (intPart, fracPart []byte) {
	text = trimJSON(text)
	negative := false
	start := 0
	if len(text) > 0 && (text[0] == '-' || text[0] == '+') {
		negative = text[0] == '-'
		start = 1
	}
	dot := -1
	for i := start; i < len(text); i++ {
		if text[i] == '.' {
			dot = i
			break
		}
	}
	if dot < 0 {
		intPart = text[start:]
	} else {
		intPart = text[start:dot]
		fracPart = text[dot+1:]
		for len(fracPart) > 0 && fracPart[len(fracPart)-1] == '0' {
			fracPart = fracPart[:len(fracPart)-1]
		}
	}
	if len(intPart) == 0 {
		intPart = []byte("0")
	}
	if negative && !isZeroDecimal(intPart, fracPart) {
		intPart = append([]byte("-"), intPart...)
	} else if negative {
		intPart = []byte("0")
	}
	return intPart, fracPart
}

func isZeroDecimal(intPart, fracPart []byte) bool {
	if len(fracPart) > 0 {
		return false
	}
	for _, c := range intPart {
		if c != '0' && c != '-' {
			return false
		}
	}
	return true
}

// compareDecimal compares two decimal literals exactly, returning -1, 0, or 1:
// integer parts through their exact digit length, fractions padded to equal
// length, signs applied last.
func compareDecimal(a, b []byte) int {
	aInt, aFrac := splitDecimalParts(a)
	bInt, bFrac := splitDecimalParts(b)

	aNeg := bytesStartsWith(aInt, "-")
	bNeg := bytesStartsWith(bInt, "-")
	if aNeg != bNeg {
		if aNeg {
			return -1
		}
		return 1
	}

	amag := aInt
	if aNeg {
		amag = aInt[1:]
	}
	bmag := bInt
	if bNeg {
		bmag = bInt[1:]
	}
	amag = stripLeadingZeros(amag)
	bmag = stripLeadingZeros(bmag)

	cmp := 0
	switch {
	case len(amag) < len(bmag):
		cmp = -1
	case len(amag) > len(bmag):
		cmp = 1
	default:
		cmp = stringCompare(amag, bmag)
	}
	if cmp == 0 {
		for i := 0; i < len(aFrac) || i < len(bFrac); i++ {
			var x, y byte
			if i < len(aFrac) {
				x = aFrac[i]
			}
			if i < len(bFrac) {
				y = bFrac[i]
			}
			if x != y {
				if x < y {
					cmp = -1
				} else {
					cmp = 1
				}
				break
			}
		}
	}
	if aNeg {
		return -cmp
	}
	return cmp
}

func stripLeadingZeros(text []byte) []byte {
	start := 0
	for start < len(text)-1 && text[start] == '0' {
		start++
	}
	return text[start:]
}

func stringCompare(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

func bytesStartsWith(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == prefix
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

func isJSONNumber(trimmed []byte) bool {
	if len(trimmed) == 0 {
		return false
	}
	start := 0
	if trimmed[0] == '-' || trimmed[0] == '+' {
		if len(trimmed) == 1 {
			return false
		}
		start = 1
	}
	seenDigit, seenDot := false, false
	for i := start; i < len(trimmed); i++ {
		switch {
		case trimmed[i] >= '0' && trimmed[i] <= '9':
			seenDigit = true
		case trimmed[i] == '.' && !seenDot:
			seenDot = true
		default:
			return false
		}
	}
	return seenDigit
}

// jsonValuesEqual reports whether two JSON literals denote the same value,
// comparing composite values through their canonical encodings.
func jsonValuesEqual(a, b json.RawMessage) bool {
	ca, errA := canonical(a)
	cb, errB := canonical(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(ca) == string(cb)
}
