package conformance

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// observe checks the wire invariants every adapted fixture request must meet.
func observe(t *testing.T, fx Fixture, header http.Header, body []byte) {
	t.Helper()
	var rpc rpcBody
	if err := json.Unmarshal(body, &rpc); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if rpc.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q", rpc.JSONRPC)
	}
	if rpc.Method != fx.Method() {
		t.Errorf("method = %q, fixture %q", rpc.Method, fx.Method())
	}
	if forbiddenMethod(rpc.Method) {
		t.Errorf("forbidden method %q was emitted", rpc.Method)
	}
	if header.Get("Mcp-Session-Id") != "" {
		t.Error("Mcp-Session-Id was emitted")
	}
	if header.Get("MCP-Protocol-Version") != ProtocolVersion {
		t.Errorf("MCP-Protocol-Version = %q", header.Get("MCP-Protocol-Version"))
	}
	if header.Get("Mcp-Method") != rpc.Method {
		t.Errorf("Mcp-Method %q disagrees with body method %q", header.Get("Mcp-Method"), rpc.Method)
	}
	if header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", header.Get("Content-Type"))
	}
	if header.Get("Accept") != "application/json, text/event-stream" {
		t.Errorf("Accept = %q", header.Get("Accept"))
	}
	if !strings.HasPrefix(header.Get("Authorization"), "Bearer ") || header.Get("Authorization") == "Bearer " {
		t.Errorf("Authorization = %q", header.Get("Authorization"))
	}
	if len(header.Values("Authorization")) != 1 {
		t.Errorf("Authorization count = %d", len(header.Values("Authorization")))
	}
	assertParamHeaders(t, fx, header)
	var params rpcParams
	if len(rpc.Params) > 0 && json.Unmarshal(rpc.Params, &params) != nil {
		t.Fatal("params are not an object")
	}
	if params.Task != nil {
		t.Error("params.task augmentation was emitted")
	}
	assertNameAgrees(t, header.Get("Mcp-Name"), params)
	assertMetaAgrees(t, fx, params, rpc.Params)
}

func forbiddenMethod(method string) bool {
	switch method {
	case "initialize", "notifications/initialized", "tasks/result", "tasks/list":
		return true
	default:
		return false
	}
}

func assertNameAgrees(t *testing.T, encoded string, params rpcParams) {
	t.Helper()
	want := params.Name
	if params.TaskID != "" {
		want = params.TaskID
	}
	if want == "" {
		if encoded != "" {
			t.Errorf("Mcp-Name %q set for a method without a name", encoded)
		}
		return
	}
	got, err := decodeHeader(encoded)
	if err != nil {
		t.Fatalf("decode Mcp-Name: %v", err)
	}
	if got != want {
		t.Errorf("Mcp-Name %q, body name %q", got, want)
	}
}

func assertMetaAgrees(t *testing.T, fx Fixture, params rpcParams, paramsRaw json.RawMessage) {
	t.Helper()
	if !jsonObject(params.Meta.Capabilities) {
		t.Fatal("request _meta is missing client capabilities")
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(paramsRaw, &raw) != nil {
		t.Fatal("params are not an object")
	}
	var meta struct {
		ProtocolVersion string `json:"io.modelcontextprotocol/protocolVersion"`
	}
	if json.Unmarshal(raw["_meta"], &meta) != nil || meta.ProtocolVersion != ProtocolVersion {
		t.Errorf("_meta protocolVersion = %s", raw["_meta"])
	}
	if mustDeclareTasks(fx, params.Name) && !capabilitiesDeclareTasks(params.Meta.Capabilities) {
		t.Errorf("applicable request omitted the Tasks capability: %s", params.Meta.Capabilities)
	}
}

func mustDeclareTasks(fx Fixture, tool string) bool {
	switch fx.Method() {
	case "tasks/get", "tasks/update", "tasks/cancel", "subscriptions/listen":
		return true
	case "tools/call":
		return fx.declaresTasks() || tool == "task_required"
	default:
		return false
	}
}

func capabilitiesDeclareTasks(raw json.RawMessage) bool {
	var caps struct {
		Extensions map[string]json.RawMessage `json:"extensions"`
	}
	if json.Unmarshal(raw, &caps) != nil {
		return false
	}
	ext, ok := caps.Extensions["io.modelcontextprotocol/tasks"]
	return ok && jsonObject(ext)
}

func decodeHeader(encoded string) (string, error) {
	if !strings.HasPrefix(encoded, "=?base64?") || !strings.HasSuffix(encoded, "?=") {
		return encoded, nil
	}
	raw := strings.TrimSuffix(strings.TrimPrefix(encoded, "=?base64?"), "?=")
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}
