package catalog

import (
	"strings"
	"testing"
)

const headersSchema = `{
  "type": "object",
  "properties": {
    "enabled": {"type": "boolean", "x-mcp-header": "Enabled"},
    "region": {"type": "string", "x-mcp-header": "Region"},
    "routing": {
      "type": "object",
      "properties": {
        "shard": {"type": "integer", "x-mcp-header": "Shard"}
      }
    }
  }
}`

func TestParamHeadersFromPinnedSchema(t *testing.T) {
	headers, err := ParamHeaders([]byte(headersSchema))
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 3 {
		t.Fatalf("headers = %+v", headers)
	}
	if headers[0].Name != "Enabled" || headers[0].Type != "boolean" || strings.Join(headers[0].Path, ".") != "enabled" {
		t.Fatalf("enabled = %+v", headers[0])
	}
	if headers[2].Name != "Shard" || strings.Join(headers[2].Path, ".") != "routing.shard" {
		t.Fatalf("shard = %+v", headers[2])
	}
	if err := CheckInputSchema([]byte(headersSchema)); err != nil {
		t.Fatal(err)
	}
}

func TestParamHeadersRejectUnreachableDuplicateAndWrongType(t *testing.T) {
	cases := []struct {
		name   string
		schema string
	}{
		{"root annotation", `{"type":"object","x-mcp-header":"Root"}`},
		{"items annotation", `{"type":"object","properties":{"values":{"type":"array","items":{"type":"string","x-mcp-header":"Value"}}}}`},
		{"duplicate name", `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"Region"},"b":{"type":"string","x-mcp-header":"region"}}}`},
		{"wrong type", `{"type":"object","properties":{"flag":{"type":"object","x-mcp-header":"Flag"}}}`},
		{"bad token", `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"Not A Token"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParamHeaders([]byte(tc.schema)); err == nil {
				t.Fatal("invalid annotation accepted")
			}
			if err := CheckInputSchema([]byte(tc.schema)); err == nil {
				t.Fatal("invalid input schema accepted")
			}
		})
	}
}
