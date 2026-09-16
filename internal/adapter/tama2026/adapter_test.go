package tama2026

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/upstream"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// pinnedInstructions is the profile-pinned instruction text.
const pinnedInstructions = "Pinned instructions."

// messageToolSchema is the pinned input schema for the message tool.
const messageToolSchema = `{"type":"object","properties":{"message":{"type":"string","minLength":1}},"required":["message"],"additionalProperties":false}`

const messageOutSchema = `{"type":"object","properties":{"status":{"type":"string"}},"required":["status"]}`

// pinnedMessage builds a valid pinned descriptor for the message tool.
func pinnedMessage(strategy catalog.Strategy, taskSupport catalog.TaskSupport) catalog.Descriptor {
	d := catalog.Descriptor{
		Name:         "message",
		Description:  "Send a message to Tama",
		InputSchema:  json.RawMessage(messageToolSchema),
		OutputSchema: json.RawMessage(messageOutSchema),
		TaskSupport:  taskSupport,
		Strategy:     strategy,
	}
	digest, err := d.ComputeDigest()
	if err != nil {
		panic(err)
	}
	d.Digest = digest
	return d
}

// tamaServer is a fixture TamaMCP 2026 upstream.
type tamaServer struct {
	ts              *httptest.Server
	discoverDoc     string
	tools           []string
	callResultDoc   string
	unauthorized    bool
	callReqMu       sync.Mutex
	callRaw         []byte
	callContentType string
}

func newTamaServer(t *testing.T) *tamaServer {
	t.Helper()
	s := &tamaServer{
		discoverDoc:     defaultDiscoverDoc(true, pinnedInstructions),
		tools:           []string{defaultMessageTool()},
		callResultDoc:   defaultTaskResultDoc(),
		callContentType: "application/json",
	}
	s.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.unauthorized {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body := make([]byte, 4096)
		n, _ := r.Body.Read(body)
		id := extractID(body[:n])
		switch r.URL.Path {
		case "/mcp/app":
			// Distinguish by method: the body carries it.
			var envelope struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body[:n], &envelope)
			switch envelope.Method {
			case "server/discover":
				replyJSON(t, w, id, s.discoverDoc)
			case "tools/list":
				replyJSON(t, w, id, fmt.Sprintf(`{"resultType":"complete","tools":[%s]}`, strings.Join(s.tools, ",")))
			case "tools/call":
				s.callReqMu.Lock()
				s.callRaw = append([]byte(nil), body[:n]...)
				s.callReqMu.Unlock()
				w.Header().Set("Content-Type", s.callContentType)
				replyJSON(t, w, id, s.callResultDoc)
			default:
				t.Errorf("unexpected method %q", envelope.Method)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.ts.Close)
	return s
}

// replyJSON wraps a result document in a JSON-RPC response.
func replyJSON(t *testing.T, w http.ResponseWriter, id, doc string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":`+id+`,"result":`+doc+`}`)
}

// extractID pulls the JSON-RPC id from a request body, quoted.
func extractID(body []byte) string {
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return `"x"`
	}
	return `"` + v.ID + `"`
}

func defaultDiscoverDoc(tasksExtension bool, instructions string) string {
	extensions := ``
	if tasksExtension {
		extensions = `,"extensions":{"io.modelcontextprotocol/tasks":{}}`
	}
	return fmt.Sprintf(`{
		"resultType": "complete",
		"supportedVersions": ["2026-07-28"],
		"capabilities": {"tools":{}%s},
		"instructions": %q,
		"_meta": {"io.modelcontextprotocol/serverInfo": {"name": "tama", "version": "1.0.0"}}
	}`, extensions, instructions)
}

// defaultMessageTool is the raw live tool object for the message tool.
func defaultMessageTool() string {
	return fmt.Sprintf(`{
		"name": "message",
		"description": "Send a message to Tama",
		"inputSchema": %s,
		"outputSchema": %s
	}`, messageToolSchema, messageOutSchema)
}

func defaultTaskResultDoc() string {
	return `{"resultType":"task","taskId":"task-1","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:00Z","ttlMs":86400000,"pollIntervalMs":1000}`
}

// newTestAdapter wires an adapter against the fixture server.
func newTestAdapter(t *testing.T, server *tamaServer, desc catalog.Descriptor) *Adapter {
	t.Helper()
	endpoint := server.ts.URL + "/mcp/app"
	up, err := upstream.New(upstream.Config{
		Endpoint:           endpoint,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "test-token", nil },
		MaxResponseBytes:   1 << 20,
		HTTPClient:         &http.Client{},
	})
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}
	adapter, err := New(Config{
		Profile: profile.Profile{
			Version:      1,
			Name:         profile.Name("test"),
			Origin:       server.ts.URL,
			Endpoint:     endpoint,
			Issuer:       server.ts.URL + "/oauth",
			Instructions: pinnedInstructions,
			Bounds:       profile.Bounds{ProtocolMin: "2026-07-28", ProtocolMax: "2026-07-28"},
			Operations:   []catalog.Descriptor{desc},
		},
		Upstream:       up,
		AdapterVersion: "0.2.0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return adapter
}

func TestConnectHappy(t *testing.T) {
	server := newTamaServer(t)
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired))

	cn, err := adapter.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !cn.HasTaskExtension() {
		t.Error("tasks extension not detected")
	}
	if got := cn.Catalog().Names(); len(got) != 1 || got[0] != "message" {
		t.Errorf("effective catalog = %v", got)
	}
	snap := cn.Snapshot()
	if snap.ProtocolVersion != "2026-07-28" || snap.ServerName != "tama" || snap.ServerVersion != "1.0.0" || snap.AdapterVersion != "0.2.0" {
		t.Errorf("snapshot = %+v", snap)
	}
	if !strings.Contains(string(snap.ServerCapabilities), "tools") {
		t.Error("snapshot lost server capabilities")
	}
}

func TestConnectTaskCapabilityAbsent(t *testing.T) {
	server := newTamaServer(t)
	server.discoverDoc = defaultDiscoverDoc(false, pinnedInstructions)
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired))

	if _, err := adapter.Connect(context.Background()); !errors.Is(err, ErrCatalogMismatch) {
		t.Fatalf("err = %v, want ErrCatalogMismatch", err)
	}
}

func TestConnectExtraLiveToolsIgnored(t *testing.T) {
	server := newTamaServer(t)
	extra := fmt.Sprintf(`{"name":"brand_new_tool","inputSchema":%s,"outputSchema":%s}`, messageToolSchema, messageOutSchema)
	server.tools = append(server.tools, extra)
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired))

	cn, err := adapter.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := cn.Catalog().Names(); len(got) != 1 || got[0] != "message" {
		t.Errorf("effective catalog exposed an unpinned tool: %v", got)
	}
}

func TestConnectMissingApprovedTool(t *testing.T) {
	server := newTamaServer(t)
	server.tools = nil
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired))

	if _, err := adapter.Connect(context.Background()); !errors.Is(err, ErrCatalogMismatch) {
		t.Fatalf("err = %v, want ErrCatalogMismatch", err)
	}
}

func TestConnectDescriptorDrift(t *testing.T) {
	server := newTamaServer(t)
	// Bump the live minLength: a distinct numeric literal is drift.
	server.tools = []string{strings.Replace(defaultMessageTool(), `"minLength":1`, `"minLength":2`, 1)}
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired))

	_, err := adapter.Connect(context.Background())
	if !errors.Is(err, ErrCatalogMismatch) {
		t.Fatalf("err = %v, want ErrCatalogMismatch", err)
	}
	if !strings.Contains(err.Error(), "input_schema") {
		t.Errorf("drift field not reported: %v", err)
	}
}

func TestConnectInstructionMismatch(t *testing.T) {
	server := newTamaServer(t)
	server.discoverDoc = defaultDiscoverDoc(true, "Different instructions.")
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired))

	if _, err := adapter.Connect(context.Background()); !errors.Is(err, ErrCatalogMismatch) {
		t.Fatalf("err = %v, want ErrCatalogMismatch", err)
	}
}

func TestConnectUnsupportedProtocol(t *testing.T) {
	server := newTamaServer(t)
	server.discoverDoc = strings.Replace(defaultDiscoverDoc(true, pinnedInstructions), `"2026-07-28"`, `"2025-11-25"`, 1)
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired))

	if _, err := adapter.Connect(context.Background()); !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("err = %v, want ErrProtocolMismatch", err)
	}
}

func TestConnectNoToolsCapability(t *testing.T) {
	server := newTamaServer(t)
	server.discoverDoc = strings.Replace(defaultDiscoverDoc(true, pinnedInstructions), `"tools":{},`, "", 1)
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired))

	if _, err := adapter.Connect(context.Background()); !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("err = %v, want ErrProtocolMismatch", err)
	}
}

func TestConnectUnauthorized(t *testing.T) {
	server := newTamaServer(t)
	server.unauthorized = true
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired))

	if _, err := adapter.Connect(context.Background()); !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("err = %v, want ErrAuthenticationRequired", err)
	}
}

func TestExecuteRequiredTask(t *testing.T) {
	server := newTamaServer(t)
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired))
	cn, err := adapter.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	resp, err := cn.Execute(context.Background(), "message", json.RawMessage(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !resp.IsTask() {
		t.Fatalf("response = %+v, want task", resp)
	}
	assertCallCapabilities(t, server, true)
}

func TestExecuteRequiredRejectsComplete(t *testing.T) {
	server := newTamaServer(t)
	server.callResultDoc = `{"resultType":"complete","content":[{"type":"text","text":"done"}]}`
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired))
	cn, err := adapter.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := cn.Execute(context.Background(), "message", json.RawMessage(`{"message":"hi"}`)); !errors.Is(err, ErrProtocolMismatch) {
		t.Fatalf("err = %v, want ErrProtocolMismatch", err)
	}
}

func TestExecuteForbiddenNeverTasks(t *testing.T) {
	server := newTamaServer(t)
	server.callResultDoc = `{"resultType":"complete","content":[{"type":"text","text":"done"}]}`
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportForbidden))
	cn, err := adapter.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := cn.Execute(context.Background(), "message", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	assertCallCapabilities(t, server, false)
}

func TestExecuteForbiddenRejectsTask(t *testing.T) {
	server := newTamaServer(t)
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportForbidden))
	cn, err := adapter.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := cn.Execute(context.Background(), "message", json.RawMessage(`{}`)); !errors.Is(err, ErrUnexpectedTaskResult) {
		t.Fatalf("err = %v, want ErrUnexpectedTaskResult", err)
	}
}

func TestExecuteOptionalAcceptsBoth(t *testing.T) {
	server := newTamaServer(t)
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportOptional))
	cn, err := adapter.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := cn.Execute(context.Background(), "message", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("task result rejected: %v", err)
	}
	server.callResultDoc = `{"resultType":"complete","content":[]}`
	if _, err := cn.Execute(context.Background(), "message", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("complete result rejected: %v", err)
	}
}

func TestExecuteLocalStrategyRejected(t *testing.T) {
	server := newTamaServer(t)
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyLocalReplayable, catalog.TaskSupportForbidden))
	cn, err := adapter.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := cn.Execute(context.Background(), "message", json.RawMessage(`{}`)); !errors.Is(err, ErrOperationNotAllowed) {
		t.Fatalf("err = %v, want ErrOperationNotAllowed", err)
	}
}

// TestExecuteLocalRejectsTaskResult pins the distinct sentinel: a task
// envelope for a synchronous tool is an operation-contract violation, not
// catalog drift.
func TestExecuteLocalRejectsTaskResult(t *testing.T) {
	server := newTamaServer(t)
	// The fixture's default tools/call reply is a task envelope.
	adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyLocalReplayable, catalog.TaskSupportForbidden))
	cn, err := adapter.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_, err = cn.ExecuteLocal(context.Background(), "message", json.RawMessage(`{}`))
	if !errors.Is(err, ErrUnexpectedTaskResult) {
		t.Fatalf("err = %v, want ErrUnexpectedTaskResult", err)
	}
	if errors.Is(err, ErrCatalogMismatch) {
		t.Fatalf("err must not report catalog drift: %v", err)
	}
}

// assertCallCapabilities checks the per-request _meta capabilities on the
// wire: the Tasks extension present exactly when the descriptor allows it.
func assertCallCapabilities(t *testing.T, server *tamaServer, wantTasks bool) {
	t.Helper()
	server.callReqMu.Lock()
	raw := make([]byte, len(server.callRaw))
	copy(raw, server.callRaw)
	server.callReqMu.Unlock()

	var envelope struct {
		Params struct {
			Meta struct {
				Capabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
			} `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode call request: %v", raw)
	}
	hasTasks := strings.Contains(string(envelope.Params.Meta.Capabilities), "io.modelcontextprotocol/tasks")
	if hasTasks != wantTasks {
		t.Errorf("tasks capability declared = %v, want %v (caps %s)", hasTasks, wantTasks, envelope.Params.Meta.Capabilities)
	}
}

func TestNewEndpointBinding(t *testing.T) {
	server := newTamaServer(t)
	other := newTamaServer(t)
	up, err := upstream.New(upstream.Config{
		Endpoint:           other.ts.URL + "/mcp/app",
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "test-token", nil },
		MaxResponseBytes:   1 << 20,
		HTTPClient:         &http.Client{},
	})
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}
	if _, err := New(Config{
		Profile: profile.Profile{
			Endpoint:   server.ts.URL + "/mcp/app",
			Operations: []catalog.Descriptor{pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired)},
		},
		Upstream:       up,
		AdapterVersion: "0.2.0",
	}); err == nil {
		t.Fatal("cross-endpoint wiring accepted")
	}
}

// TestProfileBoundsRejectStaleAndFuture proves the adapter refuses profiles
// whose compatibility bounds do not cover the pinned protocol version.
func TestProfileBoundsRejectStaleAndFuture(t *testing.T) {
	server := newTamaServer(t)
	endpoint := server.ts.URL + "/mcp/app"
	up, err := upstream.New(upstream.Config{
		Endpoint:           endpoint,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "0.1.0"},
		ClientCapabilities: json.RawMessage(`{}`),
		TokenProvider:      func(context.Context) (string, error) { return "test-token", nil },
		MaxResponseBytes:   1 << 20,
		HTTPClient:         &http.Client{},
	})
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}
	for _, tc := range []struct {
		name     string
		min, max string
	}{
		{"stale bounds", "2025-03-26", "2025-11-25"},
		{"future bounds", "2026-08-01", "2026-12-31"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{
				Profile: profile.Profile{
					Endpoint:     endpoint,
					Instructions: pinnedInstructions,
					Bounds:       profile.Bounds{ProtocolMin: tc.min, ProtocolMax: tc.max},
					Operations:   []catalog.Descriptor{pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired)},
				},
				Upstream:       up,
				AdapterVersion: "0.2.0",
			})
			if err == nil {
				t.Fatal("out-of-bounds profile accepted")
			}
		})
	}
}

// TestConnectMalformedCapabilities proves malformed capability declarations
// fail closed: null or scalar tools, an array extensions member, and a null
// Tasks extension value are not empty objects.
func TestConnectMalformedCapabilities(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(doc string) string
	}{
		{"null tools", func(doc string) string {
			return strings.Replace(doc, `"tools":{}`, `"tools":null`, 1)
		}},
		{"scalar tools", func(doc string) string {
			return strings.Replace(doc, `"tools":{}`, `"tools":"yes"`, 1)
		}},
		{"array extensions", func(doc string) string {
			return strings.Replace(doc, `"extensions":{"io.modelcontextprotocol/tasks":{}}`, `"extensions":[1]`, 1)
		}},
		{"null tasks extension value", func(doc string) string {
			return strings.Replace(doc, `"io.modelcontextprotocol/tasks":{}}`, `"io.modelcontextprotocol/tasks":null}`, 1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newTamaServer(t)
			server.discoverDoc = tc.mutate(server.discoverDoc)
			adapter := newTestAdapter(t, server, pinnedMessage(catalog.StrategyUpstreamTask, catalog.TaskSupportRequired))
			if _, err := adapter.Connect(context.Background()); !errors.Is(err, ErrProtocolMismatch) {
				t.Fatalf("err = %v, want ErrProtocolMismatch", err)
			}
		})
	}
}
