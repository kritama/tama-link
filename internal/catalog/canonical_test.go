package catalog

import (
	"encoding/json"
	"testing"
)

func TestCanonicalPreservesHighPrecisionIntegers(t *testing.T) {
	t.Parallel()

	// 2^53+1 and 2^53+3 collapse to the same float64 but must remain
	// distinct literals.
	a := json.RawMessage(`{"limit":9007199254740993}`)
	b := json.RawMessage(`{"limit":9007199254740995}`)
	if canonicalEqual(a, b) {
		t.Fatal("distinct integers above 2^53 compared equal")
	}
	c := json.RawMessage(`{"limit": 9007199254740993}`)
	if !canonicalEqual(a, c) {
		t.Fatal("identical integers with different spacing compared unequal")
	}
}

func TestCanonicalComparesExponentFormsByLiteral(t *testing.T) {
	t.Parallel()

	// Numeric equivalence is not asserted; literal differences fail closed.
	a := json.RawMessage(`{"timeout":1e2}`)
	b := json.RawMessage(`{"timeout":100}`)
	if canonicalEqual(a, b) {
		t.Fatal("different numeric literals compared equal")
	}
}

func TestCanonicalIgnoresKeyOrderAndSpacing(t *testing.T) {
	t.Parallel()

	a := json.RawMessage(`{"a":1,"b":{"c":[true,null]}}`)
	b := json.RawMessage(`  { "b" : { "c" : [ true , null ] }, "a" : 1 } `)
	if !canonicalEqual(a, b) {
		t.Fatal("equivalent objects compared unequal")
	}
}

func TestCanonicalRejectsTrailingData(t *testing.T) {
	t.Parallel()

	if _, err := canonical(json.RawMessage(`{"a":1} garbage`)); err == nil {
		t.Fatal("trailing data accepted")
	}
	if _, err := canonical(json.RawMessage(`{"a":1}{"b":2}`)); err == nil {
		t.Fatal("second value accepted")
	}
}

func TestCanonicalRejectsDuplicateKeys(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		`{"a":1,"a":2}`,
		`{"a":{"b":1,"b":2}}`,
	} {
		if _, err := canonical(json.RawMessage(raw)); err == nil {
			t.Fatalf("duplicate keys accepted in %s", raw)
		}
	}
}

func TestCanonicalRejectsInvalidJSON(t *testing.T) {
	t.Parallel()

	// An empty RawMessage means absent and canonicalizes to null.
	for _, raw := range []string{
		`{`,
		`{"a":`,
		`[1,2`,
		`"unterminated`,
	} {
		if _, err := canonical(json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid JSON %q accepted", raw)
		}
	}
}
