package tama2026

import "testing"

// TestNormalizeCompleteResultRequiresIsError pins the required-field check:
// a complete result without a usable isError is a protocol failure, never
// silently read as a success.
func TestNormalizeCompleteResultRequiresIsError(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"omitted isError", `{"resultType":"complete","content":[{"type":"text","text":"ok"}]}`},
		{"null isError", `{"resultType":"complete","isError":null,"content":[{"type":"text","text":"ok"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeCompleteResult([]byte(tc.raw))
			if err == nil {
				t.Fatalf("NormalizeCompleteResult(%s) succeeded, want a protocol failure", tc.name)
			}
		})
	}
}

func TestNormalizeCompleteResultAcceptsIsErrorValues(t *testing.T) {
	for _, isError := range []string{"false", "true"} {
		raw := `{"resultType":"complete","isError":` + isError + `,"content":[{"type":"text","text":"ok"}]}`
		result, err := NormalizeCompleteResult([]byte(raw))
		if err != nil {
			t.Fatalf("isError=%s: %v", isError, err)
		}
		if result.IsError != (isError == "true") {
			t.Fatalf("isError=%s: IsError = %v", isError, result.IsError)
		}
	}
}
