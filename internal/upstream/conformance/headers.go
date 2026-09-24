package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/textproto"
	"strings"
	"testing"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/upstream"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// headersToolSchema is the pinned TamaMCP headers tool input schema. The
// conformance gate derives argument headers from it rather than copying the
// fixture request.
const headersToolSchema = `{
  "type": "object",
  "properties": {
    "enabled": {"type": "boolean", "x-mcp-header": "Enabled"},
    "note": {"type": "string", "x-mcp-header": "Note"},
    "region": {"type": "string", "x-mcp-header": "Region"},
    "routing": {
      "type": "object",
      "properties": {
        "shard": {"type": "integer", "x-mcp-header": "Shard"}
      },
      "required": ["shard"],
      "additionalProperties": false
    }
  },
  "required": ["enabled", "region", "routing"],
  "additionalProperties": false
}`

func paramHeadersFor(name string) ([]upstream.ParamHeader, error) {
	if name != "headers" {
		return nil, nil
	}
	extracted, err := catalog.ParamHeaders(json.RawMessage(headersToolSchema))
	if err != nil {
		return nil, err
	}
	out := make([]upstream.ParamHeader, len(extracted))
	for i, header := range extracted {
		out[i] = upstream.ParamHeader{Name: header.Name, Path: append([]string(nil), header.Path...), Type: header.Type}
	}
	return out, nil
}

// assertMirroredHeaderIsRequired proves the client emits the header the
// negative fixture omits. That fixture is not replayed as a corrected
// request that happens to receive the server's rejection.
func assertMirroredHeaderIsRequired(t *testing.T, fx Fixture) {
	t.Helper()
	params, err := fx.params()
	if err != nil {
		t.Fatal(err)
	}
	headers, err := paramHeadersFor(params.Name)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	client := newClientTransport(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Mcp-Param-Enabled") == "" {
			t.Errorf("client omitted Mcp-Param-Enabled")
		}
		return nil, errStopAfterHeaders
	}))
	_, err = client.CallTool(context.Background(), &upstream.CallToolParams{
		Name:         params.Name,
		Arguments:    params.Arguments,
		ParamHeaders: headers,
	})
	if calls != 1 {
		t.Fatalf("header probe calls = %d, want 1", calls)
	}
	if err == nil {
		t.Fatal("header probe was treated as a completed fixture response")
	}
	if fixtureHeader(fx, "mcp-param-enabled") != "" {
		t.Fatal("negative fixture unexpectedly contains the required header")
	}
}

func assertParamHeaders(t *testing.T, fx Fixture, header http.Header) {
	t.Helper()
	want := fixtureParamHeaders(fx)
	got := capturedParamHeaders(header)
	if len(want) == 0 {
		if len(got) != 0 {
			t.Errorf("unexpected argument headers %v", got)
		}
		return
	}
	if len(got) != len(want) {
		t.Fatalf("argument headers %v, fixture %v", got, want)
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("header %s = %q, fixture %q", name, got[name], value)
		}
	}
}

func fixtureParamHeaders(fx Fixture) map[string]string {
	out := map[string]string{}
	for _, pair := range fx.Request.Headers {
		if len(pair) != 2 || !strings.HasPrefix(strings.ToLower(pair[0]), "mcp-param-") {
			continue
		}
		out[textproto.CanonicalMIMEHeaderKey(pair[0])] = pair[1]
	}
	return out
}

func capturedParamHeaders(header http.Header) map[string]string {
	out := map[string]string{}
	for name, values := range header {
		if !strings.HasPrefix(name, "Mcp-Param-") {
			continue
		}
		out[name] = strings.Join(values, ",")
	}
	return out
}

func fixtureHeader(fx Fixture, name string) string {
	for _, pair := range fx.Request.Headers {
		if len(pair) == 2 && strings.EqualFold(pair[0], name) {
			return pair[1]
		}
	}
	return ""
}

func newClientTransport(t *testing.T, transport http.RoundTripper) *upstream.Client {
	t.Helper()
	client, err := upstream.New(upstream.Config{
		Endpoint:           "http://127.0.0.1:9",
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.0.0-conformance"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "conformance-token", nil },
		MaxResponseBytes:   1 << 20,
		HTTPClient:         &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

var errStopAfterHeaders = errString("header probe stops before a fixture response")

type errString string

func (e errString) Error() string { return string(e) }
