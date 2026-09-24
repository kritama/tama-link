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
	"sync/atomic"
	"testing"
	"time"

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
	Headers                  http.Header
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
			Headers:                  r.Header.Clone(),
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

// newRedirectingEndpoint answers every request with one redirect to the
// target, counting how many times the target was actually reached.
func newRedirectingEndpoint(t *testing.T, status int) (endpoint string, targetHits *int32) {
	t.Helper()
	hits := new(int32)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, jsonReply("x", `{}`))
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, status)
	}))
	t.Cleanup(redirector.Close)
	return redirector.URL, hits
}

// TestRedirectsAreRejected proves a 3xx never changes destination: the
// redirect target receives zero requests for every redirect status, and the
// caller sees a classified failure.
func TestRedirectsAreRejected(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			endpoint, hits := newRedirectingEndpoint(t, status)
			client, err := New(Config{
				Endpoint:           endpoint,
				ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
				ClientCapabilities: json.RawMessage(`{}`),
				TokenProvider:      func(context.Context) (string, error) { return "t", nil },
				MaxResponseBytes:   1 << 20,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := client.Discover(context.Background(), 0); err == nil {
				t.Fatal("redirected request succeeded")
			}
			if got := atomic.LoadInt32(hits); got != 0 {
				t.Fatalf("redirect target hit %d times, want 0", got)
			}
		})
	}
}

// TestRedirectsRejectedOnAuthenticatedPost proves the refusal also applies
// to authenticated tools/call POSTs with 307/308, where following the
// redirect would replay the bearer token to a different destination.
func TestRedirectsRejectedOnAuthenticatedPost(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprintf("%d", status), func(t *testing.T) {
			endpoint, hits := newRedirectingEndpoint(t, status)
			client, err := New(Config{
				Endpoint:           endpoint,
				ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
				ClientCapabilities: json.RawMessage(`{}`),
				TokenProvider:      func(context.Context) (string, error) { return "secret-token", nil },
				MaxResponseBytes:   1 << 20,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := client.CallTool(context.Background(), &CallToolParams{Name: "message"}); err == nil {
				t.Fatal("redirected authenticated POST succeeded")
			}
			if got := atomic.LoadInt32(hits); got != 0 {
				t.Fatalf("redirect target hit %d times, want 0", got)
			}
		})
	}
}

// TestDefaultClientRefusesRedirects proves the default (caller-supplied-nil)
// client also refuses redirects, not only cloned supplied clients.
func TestDefaultClientRefusesRedirects(t *testing.T) {
	endpoint, hits := newRedirectingEndpoint(t, http.StatusFound)
	client, err := New(Config{
		Endpoint:           endpoint,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "t", nil },
		MaxResponseBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Discover(context.Background(), 0); err == nil {
		t.Fatal("default client followed a redirect")
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("redirect target hit %d times, want 0", got)
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
	_, err = client.Discover(ctx, 0)
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
	_, err = client.Discover(context.Background(), 0)
	if !IsAuth(err) {
		t.Fatalf("err = %v, want auth kind", err)
	}
	if len(ts.requests) != 0 {
		t.Fatalf("%d requests reached the endpoint", len(ts.requests))
	}
}

// TestTokenContentionClassifiesAsTransport covers the lease-contention
// carve-out: a token provider that reports refresh-lease contention
// (ErrTokenContended) describes transient coordination with another
// process, not a credential rejection, so it must not classify as KindAuth.
func TestTokenContentionClassifiesAsTransport(t *testing.T) {
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return 200, "application/json", jsonReply(rec.BodyID, `{}`)
	})
	client, err := New(Config{
		Endpoint:           ts.URL,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider: func(context.Context) (string, error) {
			return "", fmt.Errorf("%w: the winning process is refreshing the credential", ErrTokenContended)
		},
		MaxResponseBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = client.Discover(context.Background(), 0)
	if !IsTokenContended(err) {
		t.Fatalf("err = %v, want token contention", err)
	}
	if IsAuth(err) {
		t.Fatal("contention classified as an authentication rejection")
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
		_, err := client.Discover(context.Background(), 0)
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
		_, err := client.Discover(context.Background(), 0)
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
			_, err := client.Discover(context.Background(), 0)
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
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "plain body", contentType: "text/plain", body: "unavailable"},
		{name: "non jsonrpc error object", contentType: "application/json", body: `{"error":{"message":"upstream unavailable"}}`},
		{name: "zero jsonrpc code", contentType: "application/json", body: `{"jsonrpc":"2.0","error":{"code":0,"message":"invalid"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := newTestServer(t, func(_ *recordedRequest) (int, string, string) {
				return 503, tt.contentType, tt.body
			})
			client := newTestClient(t, ts)
			_, err := client.Discover(context.Background(), 0)
			var uerr *Error
			if !errors.As(err, &uerr) || uerr.Kind != KindHTTP || uerr.Code != 503 {
				t.Fatalf("err = %+v, want http 503", err)
			}
		})
	}
}

// TestMalformedResponses covers undecodable bodies and streams.
func TestMalformedResponses(t *testing.T) {
	t.Run("invalid json", func(t *testing.T) {
		ts := newTestServer(t, func(_ *recordedRequest) (int, string, string) {
			return 200, "application/json", `{not json`
		})
		client := newTestClient(t, ts)
		_, err := client.Discover(context.Background(), 0)
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
		_, err := client.Discover(context.Background(), 0)
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
		_, err := client.Discover(context.Background(), 0)
		var uerr *Error
		if !errors.As(err, &uerr) || uerr.Kind != KindTransport {
			t.Fatalf("err = %v, want transport kind", err)
		}
	})
}

// TestFiniteCallHonorsRequestTimeout proves ordinary requests are bounded by
// the per-request deadline even though the client carries no overall timeout.
func TestFiniteCallHonorsRequestTimeout(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-release:
		case <-time.After(500 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, jsonReply("x", `{}`))
	}))
	defer slow.Close()
	defer close(release)
	client, err := New(Config{
		Endpoint:           slow.URL,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "t", nil },
		MaxResponseBytes:   1 << 20,
		RequestTimeout:     50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	start := time.Now()
	_, err = client.Discover(context.Background(), 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Errorf("call outlived the per-request deadline by a wide margin: %s", elapsed)
	}
}

// TestSubscriptionNotBoundByRequestTimeout proves a subscription stream may
// remain open far beyond the finite-request timeout, and still closes
// promptly when the caller cancels its context.
func TestSubscriptionNotBoundByRequestTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		var envelope struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &envelope)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, `data: {"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%q},"notifications":{"taskIds":[]}}}`+"\n\n", envelope.ID)
		flusher.Flush()
		// Stay open until the client cancels.
		<-r.Context().Done()
	}))
	defer ts.Close()
	client, err := New(Config{
		Endpoint:           ts.URL,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{"extensions":{"io.modelcontextprotocol/tasks":{}}}`),
		TokenProvider:      func(context.Context) (string, error) { return "t", nil },
		MaxResponseBytes:   1 << 20,
		RequestTimeout:     50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ackedCh := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Subscribe(ctx, []string{"task-1"}, &SubscribeCallbacks{
			OnAcknowledged: func([]string) error { close(ackedCh); return nil },
			OnTask:         func(TaskState) error { return nil },
		})
	}()
	// Wait for the acknowledgement, then hold the stream open well past the
	// 50ms finite-request timeout.
	select {
	case <-ackedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never acknowledged")
	}
	time.Sleep(300 * time.Millisecond)
	select {
	case <-errCh:
		t.Fatal("stream died before the caller cancelled it")
	default:
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close promptly on cancellation")
	}
}

// TestCallToolPerRequestResponseBound pins the per-request response bound:
// a positive MaxResponseBytes on CallToolParams bounds that one response
// instead of the client's configured bound, and zero keeps the default.
// A submission's accepted lifecycle policy rides on this field, so a
// recovered execution runs under the limits it was accepted with.
func TestCallToolPerRequestResponseBound(t *testing.T) {
	big := strings.Repeat("p", 4096)
	ts := newTestServer(t, func(rec *recordedRequest) (int, string, string) {
		return http.StatusOK, "application/json",
			jsonReply(rec.BodyID, `{"resultType":"complete","isError":false,"structuredContent":{"pad":"`+big+`"}}`)
	})
	client, err := New(Config{
		Endpoint:           ts.URL,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "t", nil },
		MaxResponseBytes:   1 << 20,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The per-request bound below the response size fails even though the
	// client's configured bound is far larger.
	if _, err := client.CallTool(context.Background(), &CallToolParams{Name: "message", MaxResponseBytes: 1024}); err == nil {
		t.Fatal("call succeeded under the per-request bound, want too large")
	}

	// Zero keeps the client's configured bound: the same response succeeds.
	if _, err := client.CallTool(context.Background(), &CallToolParams{Name: "message"}); err != nil {
		t.Fatalf("call under the client bound: %v", err)
	}

	// A per-request bound above the response size also succeeds.
	if _, err := client.CallTool(context.Background(), &CallToolParams{Name: "message", MaxResponseBytes: 1 << 20}); err != nil {
		t.Fatalf("call under a larger per-request bound: %v", err)
	}
}
