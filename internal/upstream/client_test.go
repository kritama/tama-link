package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// recordedRequest captures one request the test server observed, with the
// body decoded far enough to check header/body agreement.
type recordedRequest struct {
	Method                   string
	Path                     string
	Authorization            string
	Protocol                 string
	MethodHeader             string
	NameHeader               string
	Body                     json.RawMessage
	BodyMethod               string
	BodyID                   string
	Meta                     map[string]json.RawMessage
	Params                   map[string]json.RawMessage
	AuthorizationHeaderCount int
}

type testServer struct {
	*httptest.Server
	requests []recordedRequest
}

// newTestServer builds an upstream stub. respond maps a request method to a
// responder; a missing method responds 404 with method-not-found, matching
// TamaMCP's fixed behavior.
func newTestServer(t *testing.T, respond func(rec *recordedRequest) (status int, contentType string, body string)) *testServer {
	t.Helper()
	ts := &testServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var envelope struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var idStr string
		if s, ok := envelope.ID.(string); ok {
			idStr = s
		}
		var paramsMap map[string]json.RawMessage
		_ = json.Unmarshal(envelope.Params, &paramsMap)
		var meta map[string]json.RawMessage
		if paramsMap != nil {
			_ = json.Unmarshal(paramsMap["_meta"], &meta)
		}
		rec := &recordedRequest{
			Method:                   envelope.Method,
			Path:                     r.URL.Path,
			Authorization:            r.Header.Get("Authorization"),
			Protocol:                 r.Header.Get(headerProtocolVersion),
			MethodHeader:             r.Header.Get(headerMethod),
			NameHeader:               r.Header.Get(headerName),
			Body:                     body,
			BodyMethod:               envelope.Method,
			BodyID:                   idStr,
			Meta:                     meta,
			Params:                   paramsMap,
			AuthorizationHeaderCount: len(r.Header.Values("Authorization")),
		}
		ts.requests = append(ts.requests, *rec)
		status, contentType, respBody := respond(rec)
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// newTestClient builds a client bound to the test server.
func newTestClient(t *testing.T, ts *testServer) *Client {
	t.Helper()
	client, err := New(Config{
		Endpoint:           ts.URL,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{"extensions":{"io.modelcontextprotocol/tasks":{}}}`),
		TokenProvider:      func(context.Context) (string, error) { return "test-token", nil },
		MaxResponseBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

// jsonReply renders a JSON-RPC success envelope for the request ID.
func jsonReply(id string, result string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":%s}`, id, result)
}

// jsonErrorReply renders a JSON-RPC error envelope for the request ID.
func jsonErrorReply(id string, code int, message string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"error":{"code":%d,"message":%q}}`, id, code, message)
}

// sseReply wraps a JSON-RPC payload in one SSE event. Multi-line payloads
// are framed as multiple data fields, which the SSE grammar joins with
// newlines; a single data field may not contain a bare newline.
func sseReply(payload string) string {
	var out strings.Builder
	for _, line := range strings.Split(payload, "\n") {
		out.WriteString("data: ")
		out.WriteString(line)
		out.WriteByte('\n')
	}
	out.WriteByte('\n')
	return out.String()
}

// assertRequestWire checks the shared 2026-07-28 request invariants on one
// recorded request: method header matches the body, protocol header is the
// pinned version, the bearer token was sent exactly once, and the _meta
// triple is present with matching values.
func assertRequestWire(t *testing.T, rec recordedRequest, wantMethod, wantName string) {
	t.Helper()
	if rec.MethodHeader != wantMethod {
		t.Errorf("Mcp-Method header = %q, want %q", rec.MethodHeader, wantMethod)
	}
	if rec.BodyMethod != wantMethod {
		t.Errorf("body method = %q, want %q", rec.BodyMethod, wantMethod)
	}
	if rec.Protocol != protocolVersion {
		t.Errorf("MCP-Protocol-Version header = %q, want %q", rec.Protocol, protocolVersion)
	}
	if rec.NameHeader != wantName {
		t.Errorf("Mcp-Name header = %q, want %q", rec.NameHeader, wantName)
	}
	if rec.Authorization != "Bearer test-token" {
		t.Errorf("Authorization = %q, want bearer test-token", rec.Authorization)
	}
	if rec.AuthorizationHeaderCount != 1 {
		t.Errorf("Authorization header count = %d, want 1", rec.AuthorizationHeaderCount)
	}
	if got := string(rec.Meta["io.modelcontextprotocol/protocolVersion"]); got != `"`+protocolVersion+`"` {
		t.Errorf("_meta protocolVersion = %s, want %s", got, protocolVersion)
	}
	if _, ok := rec.Meta["io.modelcontextprotocol/clientInfo"]; !ok {
		t.Error("_meta clientInfo missing")
	}
	if _, ok := rec.Meta["io.modelcontextprotocol/clientCapabilities"]; !ok {
		t.Error("_meta clientCapabilities missing")
	}
}

// assertNoLegacyMethods fails if the server saw any legacy or removed
// method on the wire.
func assertNoLegacyMethods(t *testing.T, ts *testServer) {
	t.Helper()
	forbidden := []string{"initialize", "notifications/initialized", "tasks/result", "tasks/list"}
	for _, rec := range ts.requests {
		for _, method := range forbidden {
			if rec.Method == method {
				t.Errorf("forbidden upstream method %q was sent", method)
			}
		}
	}
}

// TestNewValidation covers the config fail-closed paths.
func TestNewValidation(t *testing.T) {
	valid := Config{
		Endpoint:           "https://tama.example/mcp/app",
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "t", nil },
		MaxResponseBytes:   1024,
	}
	if _, err := New(valid); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty endpoint", func(c *Config) { c.Endpoint = "" }},
		{"relative endpoint", func(c *Config) { c.Endpoint = "/mcp/app" }},
		{"non-http scheme", func(c *Config) { c.Endpoint = "file:///etc/passwd" }},
		{"no client name", func(c *Config) { c.ClientInfo.Name = "" }},
		{"no client version", func(c *Config) { c.ClientInfo.Version = "" }},
		{"non-object capabilities", func(c *Config) { c.ClientCapabilities = json.RawMessage(`[1]`) }},
		{"missing token provider", func(c *Config) { c.TokenProvider = nil }},
		{"zero response bound", func(c *Config) { c.MaxResponseBytes = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

// TestRedirectsAreRejected proves a 3xx never changes destination.
func TestRedirectsAreRejected(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, jsonReply("x", `{}`))
	}))
	defer target.Close()
	redirector := newTestServer(t, func(_ *recordedRequest) (int, string, string) {
		return http.StatusFound, "text/plain", ""
	})
	redirector.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	})
	client, err := New(Config{
		Endpoint:           redirector.URL,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "t", nil },
		MaxResponseBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Discover(context.Background())
	if err == nil {
		t.Fatal("redirected request succeeded")
	}
	var uerr *Error
	if !errors.As(err, &uerr) || (uerr.Kind != KindTransport && uerr.Kind != KindHTTP) {
		t.Fatalf("error kind = %v, want transport or http", err)
	}
}

// TestCallerCancellation proves a cancelled context aborts in-flight work.
func TestCallerCancellation(t *testing.T) {
	release := make(chan struct{})
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, jsonReply("x", `{}`))
	}))
	defer blocking.Close()
	defer close(release)
	client, err := New(Config{
		Endpoint:           blocking.URL,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "t", nil },
		MaxResponseBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Discover(ctx)
	if !errors.Is(err, context.Canceled) && !isUpstreamError(err) {
		t.Fatalf("err = %v, want cancellation", err)
	}
}

func isUpstreamError(err error) bool {
	var uerr *Error
	return errors.As(err, &uerr)
}

// TestTokenFailureClassifiesAsAuth proves a token-provider failure maps to
// KindAuth without sending a request.
func TestTokenFailureClassifiesAsAuth(t *testing.T) {
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, `{}`)
	})
	client, err := New(Config{
		Endpoint:           ts.URL,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "", errors.New("keyring unavailable") },
		MaxResponseBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Discover(context.Background())
	if !IsAuth(err) {
		t.Fatalf("err = %v, want auth kind", err)
	}
	if len(ts.requests) != 0 {
		t.Fatalf("%d requests reached the endpoint", len(ts.requests))
	}
}

// TestResponseBounds covers oversized bodies and oversized SSE frames.
func TestResponseBounds(t *testing.T) {
	t.Run("json body", func(t *testing.T) {
		ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
			return 200, "application/json", jsonReply(rec.BodyID, `{"pad":"`+strings.Repeat("x", 3000)+`"}`)
		})
		client := newTestClient(t, ts)
		client.maxBytes = 1024
		_, err := client.Discover(context.Background())
		var uerr *Error
		if !errors.As(err, &uerr) || uerr.Kind != KindTooLarge {
			t.Fatalf("err = %v, want too-large kind", err)
		}
	})
	t.Run("sse frame", func(t *testing.T) {
		ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
			return 200, "text/event-stream", sseReply(jsonReply(rec.BodyID, `{"pad":"`+strings.Repeat("x", 3000)+`"}`))
		})
		client := newTestClient(t, ts)
		client.maxBytes = 1024
		_, err := client.Discover(context.Background())
		var uerr *Error
		if !errors.As(err, &uerr) || uerr.Kind != KindTooLarge {
			t.Fatalf("err = %v, want too-large kind", err)
		}
	})
}

// TestProtocolErrorClassification covers the fixed TamaMCP protocol errors
// and ordinary JSON-RPC errors.
func TestProtocolErrorClassification(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		code     int
		wantKind Kind
		wantCode int
	}{
		{"header mismatch", 400, -32020, KindProtocol, -32020},
		{"missing capability", 400, -32021, KindProtocol, -32021},
		{"unsupported version", 400, -32022, KindProtocol, -32022},
		{"method not found", 404, -32601, KindProtocol, -32601},
		{"jsonrpc error on 200", 200, -32602, KindProtocol, -32602},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
				return tc.status, "application/json", jsonErrorReply(rec.BodyID, tc.code, "classified")
			})
			client := newTestClient(t, ts)
			_, err := client.Discover(context.Background())
			var uerr *Error
			if !errors.As(err, &uerr) {
				t.Fatalf("err = %v, want upstream error", err)
			}
			if uerr.Kind != tc.wantKind || uerr.Code != tc.wantCode {
				t.Fatalf("kind=%v code=%d, want %v %d", uerr.Kind, uerr.Code, tc.wantKind, tc.wantCode)
			}
		})
	}
}

// TestHTTPFailureClassification covers statuses without a JSON-RPC body.
func TestHTTPFailureClassification(t *testing.T) {
	ts := newTestServer(t, func(_ *recordedRequest) (int, string, string) {
		return 503, "text/plain", "unavailable"
	})
	client := newTestClient(t, ts)
	_, err := client.Discover(context.Background())
	var uerr *Error
	if !errors.As(err, &uerr) || uerr.Kind != KindHTTP || uerr.Code != 503 {
		t.Fatalf("err = %+v, want http 503", err)
	}
}

// TestMalformedResponses covers undecodable bodies and streams.
func TestMalformedResponses(t *testing.T) {
	t.Run("invalid json", func(t *testing.T) {
		ts := newTestServer(t, func(_ *recordedRequest) (int, string, string) {
			return 200, "application/json", `{not json`
		})
		client := newTestClient(t, ts)
		_, err := client.Discover(context.Background())
		var uerr *Error
		if !errors.As(err, &uerr) || uerr.Kind != KindTransport {
			t.Fatalf("err = %v, want transport kind", err)
		}
	})
	t.Run("id mismatch", func(t *testing.T) {
		ts := newTestServer(t, func(_ *recordedRequest) (int, string, string) {
			return 200, "application/json", jsonReply("other-id", `{}`)
		})
		client := newTestClient(t, ts)
		_, err := client.Discover(context.Background())
		var uerr *Error
		if !errors.As(err, &uerr) || uerr.Kind != KindTransport {
			t.Fatalf("err = %v, want transport kind", err)
		}
	})
	t.Run("unsupported content type", func(t *testing.T) {
		ts := newTestServer(t, func(_ *recordedRequest) (int, string, string) {
			return 200, "text/html", "<html>"
		})
		client := newTestClient(t, ts)
		_, err := client.Discover(context.Background())
		var uerr *Error
		if !errors.As(err, &uerr) || uerr.Kind != KindTransport {
			t.Fatalf("err = %v, want transport kind", err)
		}
	})
}
