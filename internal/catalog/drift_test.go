package catalog

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestDriftNone(t *testing.T) {
	t.Parallel()

	d := testDescriptor(t, func(d *Descriptor) {
		d.OutputSchema = json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	})
	live := LiveTool{
		Name:         d.Name,
		InputSchema:  json.RawMessage(`{"properties":{"content":{"type":"string","description":"Message text"},"recipient":{"type":"string"}},"required":["content"],"type":"object"}`),
		OutputSchema: json.RawMessage(`{"properties":{"ok":{"type":"boolean"}},"type":"object"}`),
		Annotations:  map[string]any{},
		TaskSupport:  TaskSupportOptional,
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
					Name:         d.Name,
					InputSchema:  json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"}}}`),
					OutputSchema: d.OutputSchema,
					TaskSupport:  d.TaskSupport,
				}
			},
			want: []string{"input_schema"},
		},
		{
			name: "output schema drift",
			live: func(d Descriptor) LiveTool {
				return LiveTool{
					Name:         d.Name,
					InputSchema:  d.InputSchema,
					OutputSchema: json.RawMessage(`{"type":"object","properties":{"changed":{"type":"string"}}}`),
					TaskSupport:  d.TaskSupport,
				}
			},
			want: []string{"output_schema"},
		},
		{
			name: "annotations drift",
			live: func(d Descriptor) LiveTool {
				return LiveTool{
					Name:         d.Name,
					InputSchema:  d.InputSchema,
					OutputSchema: d.OutputSchema,
					Annotations:  map[string]any{"readOnlyHint": true},
					TaskSupport:  d.TaskSupport,
				}
			},
			want: []string{"annotations"},
		},
		{
			name: "task support drift from optional",
			live: func(d Descriptor) LiveTool {
				return LiveTool{
					Name:         d.Name,
					InputSchema:  d.InputSchema,
					OutputSchema: d.OutputSchema,
					TaskSupport:  TaskSupportForbidden,
				}
			},
			want: []string{"task_support"},
		},
		{
			name: "task support drift between optional and required",
			live: func(d Descriptor) LiveTool {
				return LiveTool{
					Name:         d.Name,
					InputSchema:  d.InputSchema,
					OutputSchema: d.OutputSchema,
					TaskSupport:  TaskSupportRequired,
				}
			},
			want: []string{"task_support"},
		},
		{
			name: "name drift",
			live: func(d Descriptor) LiveTool {
				return LiveTool{
					Name:         "renamed",
					InputSchema:  d.InputSchema,
					OutputSchema: d.OutputSchema,
					TaskSupport:  d.TaskSupport,
				}
			},
			want: []string{"name"},
		},
		{
			name: "combined drift",
			live: func(_ Descriptor) LiveTool {
				return LiveTool{
					Name:         "renamed",
					InputSchema:  json.RawMessage(`{"type":"object"}`),
					OutputSchema: json.RawMessage(`{"type":"object"}`),
					TaskSupport:  TaskSupportRequired,
				}
			},
			want: []string{"name", "input_schema", "output_schema", "task_support"},
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
