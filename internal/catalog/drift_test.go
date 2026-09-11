package catalog

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestDriftNone(t *testing.T) {
	t.Parallel()

	d := testDescriptor(t)
	live := LiveTool{
		Name:        d.Name,
		InputSchema: json.RawMessage(`{"properties":{"content":{"type":"string","description":"Message text"},"recipient":{"type":"string"}},"required":["content"],"type":"object"}`),
		Annotations: map[string]any{},
		TaskSupport: true,
	}
	if got := d.Drift(live); got != nil {
		t.Fatalf("Drift() = %v, want nil", got)
	}
}

func TestDriftReportsSecurityRelevantFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		live func(d Descriptor) LiveTool
		want []string
	}{
		{
			name: "input schema drift",
			live: func(d Descriptor) LiveTool {
				return LiveTool{
					Name:        d.Name,
					InputSchema: json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"}}}`),
					TaskSupport: true,
				}
			},
			want: []string{"input_schema"},
		},
		{
			name: "annotations drift",
			live: func(d Descriptor) LiveTool {
				return LiveTool{
					Name:        d.Name,
					InputSchema: d.InputSchema,
					Annotations: map[string]any{"readOnlyHint": true},
					TaskSupport: true,
				}
			},
			want: []string{"annotations"},
		},
		{
			name: "task support drift",
			live: func(d Descriptor) LiveTool {
				return LiveTool{
					Name:        d.Name,
					InputSchema: d.InputSchema,
					TaskSupport: false,
				}
			},
			want: []string{"task_support"},
		},
		{
			name: "name drift",
			live: func(d Descriptor) LiveTool {
				return LiveTool{
					Name:        "renamed",
					InputSchema: d.InputSchema,
					TaskSupport: true,
				}
			},
			want: []string{"name"},
		},
		{
			name: "combined drift",
			live: func(_ Descriptor) LiveTool {
				return LiveTool{
					Name:        "renamed",
					InputSchema: json.RawMessage(`{"type":"object"}`),
					TaskSupport: false,
				}
			},
			want: []string{"name", "input_schema", "task_support"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			d := testDescriptor(t)
			got := d.Drift(test.live(d))
			if !slices.Equal(got, test.want) {
				t.Fatalf("Drift() = %v, want %v", got, test.want)
			}
		})
	}
}
