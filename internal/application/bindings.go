package application

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
)

// applyBindings applies the descriptor's reviewed declarative bindings to the
// client-visible arguments and returns the complete upstream argument
// document. Only the two reviewed sources exist; targets are JSON Pointers.
// A required binding with an absent source is a validation failure.
func applyBindings(d catalog.Descriptor, in contract.SubmitInput) (json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(in.Arguments, &doc); err != nil {
		return nil, fmt.Errorf("arguments are not a JSON object: %w", err)
	}
	for _, b := range d.Bindings {
		var value string
		switch b.Source {
		case catalog.SourceClientRequestID:
			value = in.ClientRequestID
		case catalog.SourceClientContextThreadID:
			if in.ClientContext == nil {
				value = ""
			} else {
				value = in.ClientContext.ThreadID
			}
		default:
			// Unreachable: descriptor validation rejects unreviewed sources.
			return nil, fmt.Errorf("binding for %q uses an unreviewed source", b.Target)
		}
		if value == "" {
			if b.Required {
				return nil, fmt.Errorf("the selected bindings require %s", sourceLabel(b.Source))
			}
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode binding value: %w", err)
		}
		if err := setPointer(doc, b.Target, encoded); err != nil {
			return nil, err
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode upstream arguments: %w", err)
	}
	return out, nil
}

func sourceLabel(source string) string {
	if source == catalog.SourceClientContextThreadID {
		return "client_context.thread_id"
	}
	return "client_request_id"
}

// setPointer sets one JSON member at the RFC 6901 pointer, creating absent
// object members on the path. Array references are not part of the reviewed
// target space and fail closed.
func setPointer(doc map[string]json.RawMessage, pointer string, value json.RawMessage) error {
	if !strings.HasPrefix(pointer, "/") {
		return fmt.Errorf("binding target %q is not an absolute pointer", pointer)
	}
	refs := make([]string, 0, 8)
	for _, rawRef := range strings.Split(pointer[1:], "/") {
		refs = append(refs, strings.ReplaceAll(strings.ReplaceAll(rawRef, "~1", "/"), "~0", "~"))
	}
	return setPointerAt(doc, refs, value)
}

func setPointerAt(obj map[string]json.RawMessage, refs []string, value json.RawMessage) error {
	ref := refs[0]
	if len(refs) == 1 {
		obj[ref] = value
		return nil
	}
	next, ok := obj[ref]
	if !ok {
		encoded, err := json.Marshal(map[string]json.RawMessage{})
		if err != nil {
			return fmt.Errorf("create binding path: %w", err)
		}
		next = encoded
	}
	var child map[string]json.RawMessage
	if err := json.Unmarshal(next, &child); err != nil {
		return fmt.Errorf("binding path does not descend through an object at %q", ref)
	}
	if err := setPointerAt(child, refs[1:], value); err != nil {
		return err
	}
	encoded, err := json.Marshal(child)
	if err != nil {
		return fmt.Errorf("encode binding path: %w", err)
	}
	obj[ref] = encoded
	return nil
}
