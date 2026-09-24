package upstream

import (
	"net/http"
	"testing"
)

func TestRenderParamHeadersMatchesPinnedFixture(t *testing.T) {
	headers := []ParamHeader{
		{Name: "Enabled", Path: []string{"enabled"}, Type: "boolean"},
		{Name: "Region", Path: []string{"region"}, Type: "string"},
		{Name: "Shard", Path: []string{"routing", "shard"}, Type: "integer"},
		{Name: "Note", Path: []string{"note"}, Type: "string"},
	}
	args := []byte(`{"enabled":true,"region":"Hello, 世界","routing":{"shard":42}}`)
	got, err := renderParamHeaders(args, headers)
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("Mcp-Param-Enabled") != "true" {
		t.Fatalf("enabled = %q", got.Get("Mcp-Param-Enabled"))
	}
	if got.Get("Mcp-Param-Region") != "=?base64?SGVsbG8sIOS4lueVjA==?=" {
		t.Fatalf("region = %q", got.Get("Mcp-Param-Region"))
	}
	if got.Get("Mcp-Param-Shard") != "42" {
		t.Fatalf("shard = %q", got.Get("Mcp-Param-Shard"))
	}
	if got.Get("Mcp-Param-Note") != "" {
		t.Fatal("absent argument was rendered")
	}
	if _, err := renderParamHeaders([]byte(`{"shard":9007199254740992}`), []ParamHeader{{Name: "Shard", Path: []string{"shard"}, Type: "integer"}}); err == nil {
		t.Fatal("unsafe integer rendered")
	}
}

func TestRenderParamHeadersRejectsWrongType(t *testing.T) {
	_, err := renderParamHeaders([]byte(`{"enabled":"yes"}`), []ParamHeader{{Name: "Enabled", Path: []string{"enabled"}, Type: "boolean"}})
	if err == nil {
		t.Fatal("string accepted as boolean header")
	}
	if http.CanonicalHeaderKey("Mcp-Param-Enabled") != "Mcp-Param-Enabled" {
		t.Fatal("header canonicalization changed")
	}
}
