package catalog

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnyOfMatchesOneBranch(t *testing.T) {
	t.Parallel()
	schema := json.RawMessage(`{"anyOf":[{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false},{"type":"object","properties":{"error":{"type":"string"}},"required":["error"],"additionalProperties":false}]}`)
	if err := CheckSchemaVocabulary(schema); err != nil {
		t.Fatalf("vocabulary: %v", err)
	}
	if err := ValidateAgainstSchema(schema, json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("valid branch: %v", err)
	}
	if err := ValidateAgainstSchema(schema, json.RawMessage(`{"nope":1}`)); err == nil {
		t.Fatal("accepted a value that matches no branch")
	}
}

func TestAnyOfRejectsEmptyList(t *testing.T) {
	t.Parallel()
	err := CheckSchemaVocabulary(json.RawMessage(`{"anyOf":[]}`))
	if err == nil || !strings.Contains(err.Error(), "anyOf") {
		t.Fatalf("error = %v", err)
	}
}
