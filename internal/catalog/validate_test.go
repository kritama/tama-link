package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func validateTable(t *testing.T, schema, value string, wantErr string) {
	t.Helper()
	err := ValidateAgainstSchema(json.RawMessage(schema), json.RawMessage(value))
	if wantErr == "" {
		if err != nil {
			t.Fatalf("value %s against %s: unexpected error %v", value, schema, err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("value %s against %s: err = %v, want containing %q", value, schema, err, wantErr)
	}
}

func TestValidateAgainstSchemaTypes(t *testing.T) {
	for _, tc := range []struct {
		schema, value, wantErr string
	}{
		{`{"type":"object"}`, `{}`, ""},
		{`{"type":"object"}`, `[]`, "type"},
		{`{"type":"array"}`, `[]`, ""},
		{`{"type":"string"}`, `"x"`, ""},
		{`{"type":"string"}`, `1`, "type"},
		{`{"type":"boolean"}`, `true`, ""},
		{`{"type":"null"}`, `null`, ""},
		{`{"type":"null"}`, `0`, "type"},
		{`{"type":"number"}`, `1.5`, ""},
		{`{"type":"integer"}`, `7`, ""},
		{`{"type":"integer"}`, `7.0`, ""},
		{`{"type":"integer"}`, `7.5`, "type"},
		{`{"type":"integer"}`, `-3`, ""},
		{`{"type":["string","number"]}`, `"a"`, ""},
		{`{"type":["string","number"]}`, `2`, ""},
		{`{"type":["string","number"]}`, `true`, "type"},
	} {
		validateTable(t, tc.schema, tc.value, tc.wantErr)
	}
}

func TestValidateAgainstSchemaObjects(t *testing.T) {
	schema := `{
		"type": "object",
		"required": ["message"],
		"properties": {
			"message": {"type": "string", "minLength": 1, "maxLength": 32},
			"count": {"type": "integer", "minimum": 0, "maximum": 10},
			"tags": {"type": "array", "items": {"type": "string"}, "maxItems": 2},
			"mode": {"enum": ["a", "b"]},
			"fixed": {"const": 3}
		},
		"additionalProperties": false
	}`
	for _, tc := range []struct {
		value, wantErr string
	}{
		{`{"message":"hi"}`, ""},
		{`{"message":"hi","count":10,"tags":["a","b"],"mode":"b","fixed":3}`, ""},
		{`{}`, "required property \"message\""},
		{`{"message":""}`, "shorter than 1"},
		{`{"message":"` + strings.Repeat("x", 33) + `"}`, "longer than 32"},
		{`{"message":"hi","count":-1}`, "below the pinned minimum"},
		{`{"message":"hi","count":11}`, "above the pinned maximum"},
		{`{"message":"hi","count":2.5}`, "type"},
		{`{"message":"hi","tags":["a","b","c"]}`, "more than 2"},
		{`{"message":"hi","tags":[1]}`, "type"},
		{`{"message":"hi","mode":"c"}`, "enum"},
		{`{"message":"hi","fixed":4}`, "const"},
		{`{"message":"hi","extra":1}`, "unapproved property"},
		{`"scalar"`, "type"},
	} {
		validateTable(t, schema, tc.value, tc.wantErr)
	}
}

func TestValidateAgainstSchemaNumbersExact(t *testing.T) {
	// Exact decimal comparison: float64 rounding must not let a value just
	// above the bound through.
	schema := `{"type":"number","maximum":9007199254740991.5}`
	validateTable(t, schema, `9007199254740991.49`, "")
	validateTable(t, schema, `9007199254740991.51`, "above the pinned maximum")

	schema = `{"type":"number","minimum":-0.1}`
	validateTable(t, schema, `-0.1`, "")
	validateTable(t, schema, `-0.100001`, "below the pinned minimum")
	validateTable(t, schema, `0`, "")
}

func TestValidateAgainstSchemaPattern(t *testing.T) {
	schema := `{"type":"string","pattern":"^[a-z-]+$"}`
	validateTable(t, schema, `"a-b"`, "")
	validateTable(t, schema, `"A"`, "pattern")
}

func TestValidateAgainstSchemaIgnoresUnknownKeywords(t *testing.T) {
	// Keywords outside the reviewed subset must not reject a matching value.
	schema := `{"type":"object","$id":"x","description":"y","default":{}}`
	validateTable(t, schema, `{}`, "")
}

func TestValidateAgainstSchemaRejectsMalformedSchema(t *testing.T) {
	if err := ValidateAgainstSchema(json.RawMessage(`[]`), json.RawMessage(`{}`)); err == nil {
		t.Fatal("non-object schema accepted")
	}
	if err := ValidateAgainstSchema(json.RawMessage(`{"type":"object","properties":{"a":1}}`), json.RawMessage(`{"a":1}`)); err == nil {
		t.Fatal("malformed property schema accepted")
	}
}
