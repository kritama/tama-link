package catalog

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	maxTitleBytes       = 128
	maxDescriptionBytes = 1024
	maxSchemaBytes      = 64 * 1024
)

// Strategy selects the execution strategy for one operation.
type Strategy string

// Execution strategies.
const (
	StrategyUpstreamTask    Strategy = "upstream_task"
	StrategyLocalReplayable Strategy = "local_replayable"
	StrategyLocalGuarded    Strategy = "local_guarded"
	StrategyUnsupported     Strategy = "unsupported"
)

// TaskSupport is an operation's task execution declaration.
type TaskSupport string

// Task support states.
const (
	TaskSupportForbidden TaskSupport = "forbidden"
	TaskSupportOptional  TaskSupport = "optional"
	TaskSupportRequired  TaskSupport = "required"
)

// Valid reports whether the task support state is known.
func (s TaskSupport) Valid() bool {
	switch s {
	case TaskSupportForbidden, TaskSupportOptional, TaskSupportRequired:
		return true
	default:
		return false
	}
}

// Binding source vocabulary. Bindings use reviewed sources and JSON Pointer
// targets rather than arbitrary executable transformations.
const (
	SourceClientRequestID       = "client_request_id"
	SourceClientContextThreadID = "client_context.thread_id"
)

// Binding maps one reviewed source value to a JSON Pointer target in the
// upstream argument document.
type Binding struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	Required bool   `json:"required"`
}

// Valid reports whether the binding source is reviewed and the target is a
// syntactically valid RFC 6901 JSON Pointer.
func (b Binding) Valid() bool {
	switch b.Source {
	case SourceClientRequestID, SourceClientContextThreadID:
	default:
		return false
	}
	return validPointer(b.Target)
}

// validPointer reports whether p is a syntactically valid JSON Pointer with
// at least the leading slash, with well-formed ~0 and ~1 escapes.
func validPointer(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	for _, reference := range strings.Split(p[1:], "/") {
		for i := 0; i < len(reference); i++ {
			if reference[i] != '~' {
				continue
			}
			if i+1 >= len(reference) || (reference[i+1] != '0' && reference[i+1] != '1') {
				return false
			}
			i++
		}
	}
	return true
}

// Descriptor is one pinned approved upstream operation. The digest covers
// every field except the digest itself, so any change to a security-relevant
// descriptor is detectable.
type Descriptor struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	ClientSchema json.RawMessage `json:"client_schema,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Annotations  map[string]any  `json:"annotations,omitempty"`
	TaskSupport  TaskSupport     `json:"task_support"`
	Bindings     []Binding       `json:"bindings,omitempty"`
	Strategy     Strategy        `json:"strategy"`
	Digest       string          `json:"digest"`
}

// digestForm is the canonical digest input. Struct field order is fixed and
// map keys sort on marshal, so the encoding is deterministic.
type digestForm struct {
	Name         string          `json:"name"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	ClientSchema json.RawMessage `json:"client_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
	Annotations  map[string]any  `json:"annotations"`
	TaskSupport  TaskSupport     `json:"task_support"`
	Bindings     []Binding       `json:"bindings"`
	Strategy     Strategy        `json:"strategy"`
}

// ComputeDigest computes the canonical descriptor digest.
func (d Descriptor) ComputeDigest() (string, error) {
	input, err := canonical(d.InputSchema)
	if err != nil {
		return "", fmt.Errorf("canonicalize input_schema of %q: %w", d.Name, err)
	}
	client, err := canonical(d.ClientSchema)
	if err != nil {
		return "", fmt.Errorf("canonicalize client_schema of %q: %w", d.Name, err)
	}
	output, err := canonical(d.OutputSchema)
	if err != nil {
		return "", fmt.Errorf("canonicalize output_schema of %q: %w", d.Name, err)
	}
	for i := range d.Bindings {
		if !d.Bindings[i].Valid() {
			return "", fmt.Errorf("binding %d of %q has an unreviewed source or target", i, d.Name)
		}
	}
	form := digestForm{
		Name:         d.Name,
		Title:        d.Title,
		Description:  d.Description,
		InputSchema:  input,
		ClientSchema: client,
		OutputSchema: output,
		Annotations:  d.Annotations,
		TaskSupport:  d.TaskSupport,
		Bindings:     d.Bindings,
		Strategy:     d.Strategy,
	}
	encoded, err := json.Marshal(form)
	if err != nil {
		return "", fmt.Errorf("encode descriptor %q for digest: %w", d.Name, err)
	}
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", sum), nil
}

// CheckDigest verifies the pinned digest over the descriptor fields.
func (d Descriptor) CheckDigest() error {
	computed, err := d.ComputeDigest()
	if err != nil {
		return err
	}
	if computed != d.Digest {
		return fmt.Errorf("descriptor %q digest mismatch: pinned %s, computed %s", d.Name, d.Digest, computed)
	}
	return nil
}

// Validate reports whether the descriptor is structurally well-formed.
func (d Descriptor) Validate() error {
	if d.Name == "" || len(d.Name) > 128 {
		return fmt.Errorf("descriptor name must be 1-128 bytes, got %d", len(d.Name))
	}
	if len(d.Title) > maxTitleBytes {
		return fmt.Errorf("descriptor %q title exceeds %d bytes", d.Name, maxTitleBytes)
	}
	if len(d.Description) > maxDescriptionBytes {
		return fmt.Errorf("descriptor %q description exceeds %d bytes", d.Name, maxDescriptionBytes)
	}
	switch d.Strategy {
	case StrategyUpstreamTask, StrategyLocalReplayable, StrategyLocalGuarded, StrategyUnsupported:
	default:
		return fmt.Errorf("descriptor %q has unknown strategy %q", d.Name, d.Strategy)
	}
	if !d.TaskSupport.Valid() {
		return fmt.Errorf("descriptor %q has unknown task support %q", d.Name, d.TaskSupport)
	}
	if err := checkObjectSchema("input_schema", d.InputSchema, false); err != nil {
		return err
	}
	if err := checkObjectSchema("client_schema", d.ClientSchema, true); err != nil {
		return err
	}
	if err := checkObjectSchema("output_schema", d.OutputSchema, true); err != nil {
		return err
	}
	targets := make(map[string]bool, len(d.Bindings))
	for i, binding := range d.Bindings {
		if !binding.Valid() {
			return fmt.Errorf("descriptor %q binding %d has an unreviewed source or target", d.Name, i)
		}
		if targets[binding.Target] {
			return fmt.Errorf("descriptor %q has duplicate binding target %q", d.Name, binding.Target)
		}
		targets[binding.Target] = true
	}
	return d.CheckDigest()
}

func checkObjectSchema(field string, raw json.RawMessage, optional bool) error {
	if len(raw) == 0 {
		if optional {
			return nil
		}
		return fmt.Errorf("%s is required", field)
	}
	if len(raw) > maxSchemaBytes {
		return fmt.Errorf("%s exceeds %d bytes", field, maxSchemaBytes)
	}
	canonicalized, err := canonical(raw)
	if err != nil {
		return fmt.Errorf("%s is not one complete JSON value: %w", field, err)
	}
	var value any
	if err := json.Unmarshal(canonicalized, &value); err != nil {
		return fmt.Errorf("%s is not valid JSON: %w", field, err)
	}
	if _, ok := value.(map[string]any); !ok {
		return fmt.Errorf("%s must be a JSON object", field)
	}
	return nil
}
