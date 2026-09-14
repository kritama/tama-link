package jsonvalue_test

import (
	"testing"

	"github.com/kritama/tama-link/internal/jsonvalue"
)

func TestCanonicalPreservesEmptyArrays(t *testing.T) {
	t.Parallel()

	tests := []string{
		`[]`,
		`{"items":[]}`,
		`[[],null]`,
	}
	for _, input := range tests {
		input := input
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, err := jsonvalue.Canonical([]byte(input))
			if err != nil {
				t.Fatalf("Canonical: %v", err)
			}
			if string(got) != input {
				t.Fatalf("Canonical(%s) = %s, want %s", input, got, input)
			}
		})
	}
}
