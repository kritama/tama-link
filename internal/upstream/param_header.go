package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/cockroachdb/apd/v3"
)

// ParamHeader is one reviewed tools/call argument header. Callers pass only
// mappings extracted from the pinned input schema.
type ParamHeader struct {
	// Name is the x-mcp-header token.
	Name string
	// Path is the argument path, including the leaf property.
	Path []string
	// Type is string, integer, or boolean.
	Type string
}

// renderParamHeaders renders schema-declared Mcp-Param-* headers for one
// argument document. Absent and null arguments omit the header. A present
// value of the wrong type fails before the request is sent.
func renderParamHeaders(args json.RawMessage, headers []ParamHeader) (http.Header, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(args, &doc); err != nil || doc == nil {
		return nil, fmt.Errorf("tool arguments must be a JSON object")
	}
	out := make(http.Header)
	seen := make(map[string]bool, len(headers))
	for _, header := range headers {
		key := strings.ToLower(header.Name)
		if header.Name == "" || seen[key] {
			return nil, fmt.Errorf("duplicate or empty parameter header %q", header.Name)
		}
		seen[key] = true
		raw, present, err := lookupArg(doc, header.Path)
		if err != nil {
			return nil, err
		}
		if !present || string(raw) == "null" {
			continue
		}
		rendered, err := renderParamValue(header, raw)
		if err != nil {
			return nil, fmt.Errorf("parameter header %s: %w", header.Name, err)
		}
		encoded, err := encodeHeaderValue(rendered)
		if err != nil {
			return nil, err
		}
		out.Set("Mcp-Param-"+header.Name, encoded)
	}
	return out, nil
}

func lookupArg(doc map[string]json.RawMessage, path []string) (json.RawMessage, bool, error) {
	var current json.RawMessage
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, false, err
	}
	current = encoded
	for _, key := range path {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(current, &object); err != nil || object == nil {
			return nil, false, nil
		}
		next, ok := object[key]
		if !ok {
			return nil, false, nil
		}
		current = next
	}
	return current, true, nil
}

func renderParamValue(header ParamHeader, raw json.RawMessage) (string, error) {
	switch header.Type {
	case "string":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("body value is not a string")
		}
		return value, nil
	case "boolean":
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("body value is not a boolean")
		}
		if value {
			return "true", nil
		}
		return "false", nil
	case "integer":
		return renderParamInteger(raw)
	default:
		return "", fmt.Errorf("unsupported parameter type %q", header.Type)
	}
}

func renderParamInteger(raw json.RawMessage) (string, error) {
	text := strings.TrimSpace(string(raw))
	dec, _, err := apd.NewFromString(text)
	if err != nil || dec.Form != apd.Finite {
		return "", fmt.Errorf("body value is not an integer")
	}
	var reduced apd.Decimal
	reduced.Reduce(dec)
	if reduced.Exponent < 0 {
		return "", fmt.Errorf("body value is not an integer")
	}
	var limit apd.Decimal
	if _, _, err := limit.SetString(strconv.FormatInt(maxTaskSafeInt, 10)); err != nil {
		return "", err
	}
	abs := new(apd.Decimal)
	abs.Abs(dec)
	if abs.Cmp(&limit) > 0 {
		return "", fmt.Errorf("integer is outside the IEEE-754 safe range")
	}
	n, err := dec.Int64()
	if err != nil {
		return "", fmt.Errorf("integer is outside the IEEE-754 safe range")
	}
	return strconv.FormatInt(n, 10), nil
}
