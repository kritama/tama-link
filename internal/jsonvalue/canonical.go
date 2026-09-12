// Package jsonvalue provides lossless, deterministic handling of JSON values.
package jsonvalue

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Canonical returns the deterministic encoding of exactly one complete JSON
// value. Object keys are sorted, whitespace is removed, numeric literals are
// preserved, and duplicate object keys or trailing data are rejected.
func Canonical(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("null"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	value, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return json.Marshal(value)
}

func decodeValue(dec *json.Decoder) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case nil, bool, string, json.Number:
		return value, nil
	case json.Delim:
		return decodeDelimited(dec, value)
	default:
		return nil, fmt.Errorf("unsupported JSON token %T", token)
	}
}

func decodeDelimited(dec *json.Decoder, open json.Delim) (any, error) {
	switch open {
	case '[':
		var values []any
		for dec.More() {
			value, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, consumeClosing(dec, ']')
	case '{':
		values := map[string]any{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object key is not a string")
			}
			if _, exists := values[key]; exists {
				return nil, fmt.Errorf("duplicate object key %q", key)
			}
			value, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			values[key] = value
		}
		return values, consumeClosing(dec, '}')
	default:
		return nil, fmt.Errorf("unexpected delimiter %q", open)
	}
}

func consumeClosing(dec *json.Decoder, want json.Delim) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != want {
		return fmt.Errorf("unexpected token after JSON value")
	}
	return nil
}
