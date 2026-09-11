package catalog

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	// maxSignatures bounds how many operations the submit description lists.
	maxSignatures = 32
	// maxArguments bounds how many arguments one signature lists.
	maxArguments = 16
	// clippedDescription bounds one operation description in the projection.
	clippedDescription = 256
	// clippedArgumentDescription bounds one argument description.
	clippedArgumentDescription = 160
	// maxSubmitDescription bounds the rendered submit description.
	maxSubmitDescription = 8192
)

// Argument is one projected top-level argument of an operation.
type Argument struct {
	Name        string
	Type        string
	Description string
	Required    bool
}

// Signature is the bounded deterministic signature of one operation.
type Signature struct {
	Name        string
	Description string
	Arguments   []Argument
}

type schemaShape struct {
	Type        string         `json:"type"`
	Description string         `json:"description"`
	Properties  map[string]any `json:"properties"`
	Required    []string       `json:"required"`
}

// Signatures renders one signature per operation from its client-visible
// schema (falling back to the upstream schema), sorted by operation name.
func (c Catalog) Signatures() []Signature {
	operations := append([]Descriptor(nil), c.Operations...)
	sort.Slice(operations, func(i, j int) bool { return operations[i].Name < operations[j].Name })

	sigs := make([]Signature, 0, len(operations))
	for _, d := range operations {
		schema := d.ClientSchema
		if len(schema) == 0 {
			schema = d.InputSchema
		}
		sigs = append(sigs, Signature{
			Name:        d.Name,
			Description: clip(d.Description, clippedDescription),
			Arguments:   projectArguments(schema),
		})
	}
	return sigs
}

// SubmitDescription renders the bounded deterministic operation signatures
// for the downstream submit tool description.
func (c Catalog) SubmitDescription() string {
	sigs := c.Signatures()
	if len(sigs) > maxSignatures {
		sigs = sigs[:maxSignatures]
	}
	total := len(c.Operations)
	for n := len(sigs); n > 0; n-- {
		rendered := renderSignatures(sigs[:n])
		if total > n {
			rendered += fmt.Sprintf("\n\n... %d more operation(s) omitted", total-n)
		}
		if len(rendered) <= maxSubmitDescription {
			return rendered
		}
	}
	return "See the selected profile for the approved operations."
}

func renderSignatures(sigs []Signature) string {
	parts := make([]string, 0, len(sigs))
	for _, sig := range sigs {
		var b strings.Builder
		b.WriteString(sig.Name)
		if sig.Description != "" {
			b.WriteString(": ")
			b.WriteString(sig.Description)
		}
		b.WriteString(".")
		for _, arg := range sig.Arguments {
			fmt.Fprintf(&b, "\n  - %s (%s", arg.Name, arg.Type)
			if arg.Required {
				b.WriteString(", required")
			}
			b.WriteString(")")
			if arg.Description != "" {
				b.WriteString(": ")
				b.WriteString(arg.Description)
			}
		}
		parts = append(parts, b.String())
	}
	return strings.Join(parts, "\n\n")
}

func projectArguments(schema json.RawMessage) []Argument {
	var shape schemaShape
	if len(schema) == 0 || json.Unmarshal(schema, &shape) != nil {
		return nil
	}
	if shape.Type != "" && shape.Type != "object" {
		return nil
	}

	required := make(map[string]bool, len(shape.Required))
	for _, name := range shape.Required {
		required[name] = true
	}

	names := make([]string, 0, len(shape.Properties))
	for name := range shape.Properties {
		names = append(names, name)
	}
	sort.Strings(names)

	args := make([]Argument, 0, len(names))
	for _, name := range names {
		if len(args) == maxArguments {
			break
		}
		property, ok := shape.Properties[name].(map[string]any)
		if !ok {
			continue
		}
		description, _ := property["description"].(string)
		args = append(args, Argument{
			Name:        name,
			Type:        renderType(property["type"]),
			Description: clip(description, clippedArgumentDescription),
			Required:    required[name],
		})
	}
	return args
}

func renderType(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		types := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				types = append(types, s)
			}
		}
		if len(types) == 0 {
			return "any"
		}
		sort.Strings(types)
		return strings.Join(types, "|")
	default:
		return "any"
	}
}

// clip shortens s to at most n bytes on a rune boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !isRuneBoundary(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "..."
}

func isRuneBoundary(s string) bool {
	if len(s) == 0 {
		return true
	}
	last := len(s) - 1
	return s[last] < 0x80 || s[last]&0xC0 != 0x80
}
