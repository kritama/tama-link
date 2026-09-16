package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"unicode/utf8"
)

// ValidateAgainstSchema validates one JSON value against the pinned schema's
// enforced vocabulary: type, properties, required, additionalProperties
// (boolean or nested schema), items, enum, const, minimum, maximum,
// minLength, maxLength (Unicode code points), minItems, maxItems, and
// pattern (Go RE2). Pinned schemas may only use this vocabulary with
// well-formed values: CheckSchemaVocabulary enforces both at profile load,
// so nothing unenforced or malformed can reach runtime. Numbers are compared
// through their exact normalized decimal digits — exponent form included —
// never through float64.
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
	// JSON Schema string lengths count Unicode code points, not UTF-8 bytes.
	length := utf8.RuneCountInString(text)
	if s.MinLength != nil && length < *s.MinLength {
		return fmt.Errorf("%s is shorter than %d characters", path, *s.MinLength)
	}
	if s.MaxLength != nil && length > *s.MaxLength {
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
	neg, intPart, fracPart := normalizeNumber(value)
	if s.Minimum != nil && compareNormalized(neg, intPart, fracPart, s.Minimum.neg, s.Minimum.intPart, s.Minimum.fracPart) < 0 {
		return fmt.Errorf("%s is below the pinned minimum", path)
	}
	if s.Maximum != nil && compareNormalized(neg, intPart, fracPart, s.Maximum.neg, s.Maximum.intPart, s.Maximum.fracPart) > 0 {
		return fmt.Errorf("%s is above the pinned maximum", path)
	}
	return nil
}

// numberText is a JSON Schema numeric bound normalized to exact sign and
// decimal digits, so comparison never routes through float64. It accepts the
// complete JSON number grammar, including exponent form.
type numberText struct {
	neg      bool
	intPart  []byte
	fracPart []byte
}

func (n *numberText) UnmarshalJSON(raw []byte) error {
	trimmed := trimJSON(raw)
	if !isJSONNumber(trimmed) {
		return fmt.Errorf("bound is not a JSON number")
	}
	n.neg, n.intPart, n.fracPart = normalizeNumber(trimmed)
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
// fractional digits remain after the exponent is applied (1e2 and 15.0 are
// integers, 1e-1 and 1.5 are not).
func isIntegerLiteral(value []byte) bool {
	if !isJSONNumber(trimJSON(value)) {
		return false
	}
	_, _, fracPart := normalizeNumber(value)
	return len(fracPart) == 0
}

// normalizeNumber reduces one JSON number literal — any valid form, including
// exponent form — to a sign plus exact integer and fractional digits. The
// exponent is applied by shifting the decimal point with zero padding, so
// numeric comparison never routes through float64.
func normalizeNumber(value []byte) (neg bool, intPart, fracPart []byte) {
	text := trimJSON(value)
	i := 0
	if text[0] == '-' {
		neg = true
		i++
	}
	j := i
	for j < len(text) && text[j] >= '0' && text[j] <= '9' {
		j++
	}
	intDigits := text[i:j]
	var fracDigits []byte
	if j < len(text) && text[j] == '.' {
		j++
		k := j
		for k < len(text) && text[k] >= '0' && text[k] <= '9' {
			k++
		}
		fracDigits = text[j:k]
		j = k
	}
	exp := 0
	if j < len(text) && (text[j] == 'e' || text[j] == 'E') {
		expText := text[j+1:]
		eStart := 0
		expNeg := false
		if eStart < len(expText) && (expText[eStart] == '+' || expText[eStart] == '-') {
			expNeg = expText[eStart] == '-'
			eStart++
		}
		for _, c := range expText[eStart:] {
			exp = exp*10 + int(c-'0')
		}
		if expNeg {
			exp = -exp
		}
	}

	if exp >= 0 {
		if exp <= len(fracDigits) {
			intPart = append(append([]byte{}, intDigits...), fracDigits[:exp]...)
			fracPart = append([]byte{}, fracDigits[exp:]...)
		} else {
			intPart = append(append(append([]byte{}, intDigits...), fracDigits...),
				bytes.Repeat([]byte("0"), exp-len(fracDigits))...)
		}
	} else {
		shift := -exp
		if shift <= len(intDigits) {
			intPart = append([]byte{}, intDigits[:len(intDigits)-shift]...)
			fracPart = append(append([]byte{}, intDigits[len(intDigits)-shift:]...), fracDigits...)
		} else {
			intPart = []byte("0")
			fracPart = append(append(bytes.Repeat([]byte("0"), shift-len(intDigits)), intDigits...), fracDigits...)
		}
	}

	intPart = stripLeadingZeros(intPart)
	if len(intPart) == 0 {
		intPart = []byte("0")
	}
	for len(fracPart) > 0 && fracPart[len(fracPart)-1] == '0' {
		fracPart = fracPart[:len(fracPart)-1]
	}
	if isAllZeros(intPart) && len(fracPart) == 0 {
		intPart = []byte("0")
		neg = false // -0 is 0
	}
	return neg, intPart, fracPart
}

func isAllZeros(digits []byte) bool {
	for _, c := range digits {
		if c != '0' {
			return false
		}
	}
	return true
}

// compareDecimal compares two JSON number literals exactly, returning -1, 0,
// or 1, through their normalized decimal digits.
func compareDecimal(a, b []byte) int {
	aNeg, aInt, aFrac := normalizeNumber(a)
	bNeg, bInt, bFrac := normalizeNumber(b)
	return compareNormalized(aNeg, aInt, aFrac, bNeg, bInt, bFrac)
}

// compareNormalized compares two normalized numbers exactly: integer parts
// through their exact digit length, fractions padded to equal length, signs
// applied last.
func compareNormalized(aNeg bool, aInt, aFrac []byte, bNeg bool, bInt, bFrac []byte) int {
	if aNeg != bNeg {
		if aNeg {
			return -1
		}
		return 1
	}

	cmp := 0
	switch {
	case len(aInt) < len(bInt):
		cmp = -1
	case len(aInt) > len(bInt):
		cmp = 1
	default:
		cmp = stringCompare(aInt, bInt)
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
