package application

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/adapter/tama2026"
	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/limits"
	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/store"
	"github.com/kritama/tama-link/internal/upstream"
	"github.com/kritama/tama-link/internal/worker"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fixtureInstructions is the pinned instruction text shared by profile and
// fixture server so discovery matches.
const fixtureInstructions = "Fixture instructions."

// statusToolSchema and statusOutSchema pin the synchronous replayable tool.
const statusToolSchema = `{"type":"object","properties":{"detail":{"type":"string"}}}`
const statusOutSchema = `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`

// fakeTama is a fixture TamaMCP 2026 upstream with per-tool behavior.
type fakeTama struct {
	ts *httptest.Server

	calls      atomic.Int64
	fail       atomic.Bool
	taskResult atomic.Bool // status tool replies with a task result

	mu          sync.Mutex
	lastCallDoc string // JSON document of the last tools/call request
}

func newFakeTama(t *testing.T) *fakeTama {
	t.Helper()
	f := &fakeTama{}
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 65536)
		n, _ := r.Body.Read(body)
		var envelope struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body[:n], &envelope)
		id := `"` + envelope.ID + `"`

		switch envelope.Method {
		case "server/discover":
			writeFake(w, id, `{
				"resultType": "complete",
				"supportedVersions": ["2026-07-28"],
				"capabilities": {"tools":{},"extensions":{"io.modelcontextprotocol/tasks":{}}},
				"instructions": `+jsonQuote(fixtureInstructions)+`,
				"_meta": {"io.modelcontextprotocol/serverInfo": {"name": "tama", "version": "1.0.0"}}
			}`)
		case "tools/list":
			writeFake(w, id, `{"resultType":"complete","tools":[
				{"name":"status","description":"Read status","inputSchema":`+statusToolSchema+`,"outputSchema":`+statusOutSchema+`},
				{"name":"message","description":"Send a message","inputSchema":{"type":"object","properties":{"message":{"type":"string","minLength":1}},"required":["message"]},"outputSchema":{"type":"object","properties":{"status":{"type":"string"}},"required":["status"]}}
			]}`)
		case "tools/call":
			f.calls.Add(1)
			f.mu.Lock()
			f.lastCallDoc = string(body[:n])
			f.mu.Unlock()
			if f.fail.Load() {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"fixture failure"}}`, id)
				return
			}
			var params struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(envelope.Params, &params)
			switch params.Name {
			case "status":
				if f.taskResult.Load() {
					writeFake(w, id, `{"resultType":"task","taskId":"task-unexpected","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:00Z","ttlMs":86400000,"pollIntervalMs":1000}`)
					return
				}
				writeFake(w, id, `{"resultType":"complete","isError":false,
					"content":[{"type":"text","text":"status ok"}],
					"structuredContent":{"ok":true}}`)
			default:
				writeFake(w, id, `{"resultType":"complete","isError":false,
					"content":[{"type":"text","text":"sent"}],
					"structuredContent":{"status":"sent"}}`)
			}
		default:
			t.Errorf("fixture: unexpected method %q", envelope.Method)
		}
	}))
	t.Cleanup(f.ts.Close)
	return f
}

func writeFake(w http.ResponseWriter, id, doc string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, id, doc)
}

func jsonQuote(s string) string {
	q, _ := json.Marshal(s)
	return string(q)
}

// memKeys is a deterministic in-memory state key provider for tests.
type memKeys struct {
	mu   sync.Mutex
	keys map[string][]byte
}

func newMemKeys() *memKeys { return &memKeys{keys: map[string][]byte{}} }

func (m *memKeys) GetStateKey(keyID string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.keys[keyID]
	return key, ok, nil
}

func (m *memKeys) CreateStateKey() (string, []byte, error) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return "", nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id := fmt.Sprintf("key-%d", len(m.keys)+1)
	m.keys[id] = key[:]
	return id, key[:], nil
}

// fixtureProfile builds a profile pinning the fixture server's two tools:
// status (synchronous, locally replayable) and message (task-backed).
func fixtureProfile(t *testing.T, endpoint string) *profile.Profile {
	t.Helper()
	status := catalog.Descriptor{
		Name:         "status",
		Description:  "Read status",
		InputSchema:  json.RawMessage(statusToolSchema),
		OutputSchema: json.RawMessage(statusOutSchema),
		TaskSupport:  catalog.TaskSupportForbidden,
		Bindings: []catalog.Binding{
			{Source: catalog.SourceClientRequestID, Target: "/client_meta/client_request_id", Required: true},
			{Source: catalog.SourceClientContextThreadID, Target: "/client_meta/thread_id", Required: false},
		},
		Strategy: catalog.StrategyLocalReplayable,
	}
	message := catalog.Descriptor{
		Name:         "message",
		Description:  "Send a message",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","minLength":1}},"required":["message"]}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"}},"required":["status"]}`),
		TaskSupport:  catalog.TaskSupportRequired,
		Strategy:     catalog.StrategyUpstreamTask,
	}
	for _, d := range []*catalog.Descriptor{&status, &message} {
		digest, err := d.ComputeDigest()
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		d.Digest = digest
	}
	p := &profile.Profile{
		Version:      profile.SchemaVersion,
		Name:         profile.Name("fixture"),
		Origin:       strings.TrimSuffix(strings.TrimPrefix(endpoint, "http://"), "/mcp/app"),
		Endpoint:     endpoint,
		Issuer:       "http://issuer.invalid",
		Instructions: fixtureInstructions,
		Bounds:       profile.Bounds{ProtocolMin: "2026-07-28", ProtocolMax: "2026-07-28"},
		State:        profile.StateRefs{Database: "fixture", Credentials: "fixture"},
		Operations:   []catalog.Descriptor{status, message},
	}
	return p
}

// fixtureApp wires the full application stack against the fixture server.
func fixtureApp(t *testing.T, f *fakeTama) (*Service, *store.Store, *worker.Service) {
	return fixtureAppLimits(t, f, limits.Default())
}

// fixtureAppLimits wires the stack with explicit limits so tests can shrink
// the result bound below the fixture result size.
func fixtureAppLimits(t *testing.T, f *fakeTama, limitsCfg limits.Limits) (*Service, *store.Store, *worker.Service) {
	t.Helper()
	endpoint := f.ts.URL + "/mcp/app"
	p := fixtureProfile(t, endpoint)

	st, err := store.Open(context.Background(), t.TempDir()+"/state.db", newMemKeys(), store.Config{Limits: limitsCfg})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	up, err := upstream.New(upstream.Config{
		Endpoint:           endpoint,
		ClientInfo:         mcp.Implementation{Name: "tama-link", Version: "test"},
		ClientCapabilities: json.RawMessage(`{"extensions":{"io.modelcontextprotocol/tasks":{}}}`),
		TokenProvider:      func(context.Context) (string, error) { return "test-token", nil },
		MaxResponseBytes:   int64(limitsCfg.ResponseBytes),
		HTTPClient:         &http.Client{},
	})
	if err != nil {
		t.Fatalf("upstream.New: %v", err)
	}
	adapter, err := tama2026.New(tama2026.Config{
		Profile:        *p,
		Upstream:       up,
		AdapterVersion: "test-adapter",
	})
	if err != nil {
		t.Fatalf("adapter.New: %v", err)
	}
	connect := func(ctx context.Context) (*tama2026.Connection, error) { return adapter.Connect(ctx) }

	workerService, err := worker.NewService(st, NewExecutor(connect), worker.Config{
		Owner:    "fixture-worker",
		LeaseTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("worker.NewService: %v", err)
	}
	t.Cleanup(workerService.Stop)

	svc, err := New(Config{
		Profile:        p,
		Store:          st,
		Connect:        connect,
		Worker:         workerService,
		AdapterVersion: "test-adapter",
	})
	if err != nil {
		t.Fatalf("application.New: %v", err)
	}
	_ = workerService.Start(context.Background())
	return svc, st, workerService
}

// submitStatus sends one valid synchronous submit with the client context
// the fixture bindings expect, and returns its id.
func submitStatus(t *testing.T, svc *Service, clientRequestID string) string {
	t.Helper()
	out, appErr := svc.Submit(context.Background(), contract.SubmitInput{
		Tool:            "status",
		ClientRequestID: clientRequestID,
		ClientContext:   &contract.ClientContext{ThreadID: "thread-1"},
		Arguments:       json.RawMessage(`{"detail":"unit"}`),
	})
	if appErr != nil {
		t.Fatalf("submit: %s", appErr.Message)
	}
	return out.SubmissionID
}
