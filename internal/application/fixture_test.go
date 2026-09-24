package application

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
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

// noteToolSchema and noteOutSchema pin the guarded and unsupported tools.
const noteToolSchema = `{"type":"object","properties":{"note":{"type":"string"}}}`
const noteOutSchema = `{"type":"object","properties":{"status":{"type":"string"}}}`

// fakeTama is a fixture TamaMCP 2026 upstream with per-tool behavior.
type fakeTama struct {
	ts *httptest.Server

	calls      atomic.Int64
	fail       atomic.Bool
	taskResult atomic.Bool // status tool replies with a task result
	// pad, when positive, appends that many padding bytes to the status
	// tool's structured content, letting tests control the response size.
	pad atomic.Int64

	mu          sync.Mutex
	lastCallDoc string // JSON document of the last tools/call request
}

func newFakeTama(t *testing.T) *fakeTama {
	t.Helper()
	f := &fakeTama{}
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		var envelope struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(body, &envelope)
		id := `"` + envelope.ID + `"`

		switch envelope.Method {
		case "server/discover":
			writeFake(w, id, fixtureDiscoverDoc())
		case "tools/list":
			writeFake(w, id, fixtureToolsListDoc())
		case "tools/call":
			f.calls.Add(1)
			f.mu.Lock()
			f.lastCallDoc = string(body)
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
				structured := `{"ok":true}`
				if pad := f.pad.Load(); pad > 0 {
					structured = `{"ok":true,"pad":"` + strings.Repeat("p", int(pad)) + `"}`
				}
				writeFake(w, id, `{"resultType":"complete","isError":false,
					"content":[{"type":"text","text":"status ok"}],
					"structuredContent":`+structured+`}`)
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

func fixtureDiscoverDoc() string {
	return `{
		"resultType": "complete",
		"supportedVersions": ["2026-07-28"],
		"capabilities": {"tools":{},"extensions":{"io.modelcontextprotocol/tasks":{}}},
		"instructions": ` + jsonQuote(fixtureInstructions) + `,
		"_meta": {"io.modelcontextprotocol/serverInfo": {"name": "tama", "version": "1.0.0"}}
	}`
}

func fixtureToolsListDoc() string {
	return `{"resultType":"complete","tools":[
		{"name":"status","description":"Read status","inputSchema":` + statusToolSchema + `,"outputSchema":` + statusOutSchema + `},
		{"name":"message","description":"Send a message","inputSchema":{"type":"object","properties":{"message":{"type":"string","minLength":1}},"required":["message"]},"outputSchema":{"type":"object","properties":{"status":{"type":"string"}},"required":["status"]}},
		{"name":"guarded","description":"Guarded mutation","inputSchema":` + noteToolSchema + `,"outputSchema":` + noteOutSchema + `},
		{"name":"unstable","description":"Unsupported placeholder","inputSchema":` + noteToolSchema + `,"outputSchema":` + noteOutSchema + `}
	]}`
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

func newFixtureProfile(endpoint string, kind profile.Kind) (*profile.Profile, error) {
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
		Bindings: []catalog.Binding{
			{Source: catalog.SourceClientRequestID, Target: "/identifier", Required: true},
		},
		Strategy: catalog.StrategyUpstreamTask,
	}
	guarded := catalog.Descriptor{
		Name:         "guarded",
		Description:  "Guarded mutation",
		InputSchema:  json.RawMessage(noteToolSchema),
		OutputSchema: json.RawMessage(noteOutSchema),
		TaskSupport:  catalog.TaskSupportForbidden,
		Strategy:     catalog.StrategyLocalGuarded,
	}
	unstable := catalog.Descriptor{
		Name:         "unstable",
		Description:  "Unsupported placeholder",
		InputSchema:  json.RawMessage(noteToolSchema),
		OutputSchema: json.RawMessage(noteOutSchema),
		TaskSupport:  catalog.TaskSupportForbidden,
		Strategy:     catalog.StrategyUnsupported,
	}
	for _, d := range []*catalog.Descriptor{&status, &message, &guarded, &unstable} {
		digest, err := d.ComputeDigest()
		if err != nil {
			return nil, err
		}
		d.Digest = digest
	}
	base := strings.TrimSuffix(strings.TrimSuffix(endpoint, "/mcp/app"), "/mcp/system")
	path := "/mcp/system"
	operations := []catalog.Descriptor{status, guarded, unstable}
	if kind == profile.KindApp {
		path = "/mcp/app"
		operations = []catalog.Descriptor{message}
	}
	p := &profile.Profile{
		Version:      profile.SchemaVersion,
		Name:         profile.Name("fixture"),
		Origin:       strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://"),
		Endpoint:     base + path,
		Issuer:       "http://issuer.invalid",
		Instructions: fixtureInstructions,
		Bounds:       profile.Bounds{ProtocolMin: "2026-07-28", ProtocolMax: "2026-07-28"},
		State:        profile.StateRefs{Database: "fixture", Credentials: "fixture"},
		Operations:   operations,
	}
	return p, nil
}

// fixtureApp wires the full application stack against the fixture server.
func fixtureApp(t *testing.T, f *fakeTama) (*Service, *store.Store, *worker.Service) {
	return fixtureAppLimits(t, f, limits.Default())
}

// fixtureAppLimits wires the stack with explicit limits so tests can shrink
// the result bound below the fixture result size.
func fixtureAppLimits(t *testing.T, f *fakeTama, limitsCfg limits.Limits) (*Service, *store.Store, *worker.Service) {
	t.Helper()
	cfg := fixtureConfigFor(t, f, limitsCfg)
	return appFromConfig(t, cfg)
}

// fixtureConfigFor builds the fixture stack and returns its application
// Config so callers can customize it (for example the credentials probe)
// before building the Service. The store closes with the test.
func fixtureConfigFor(t *testing.T, f *fakeTama, limitsCfg limits.Limits) Config {
	t.Helper()
	return fixtureConfigWith(t, f, limitsCfg, nil)
}

func fixtureConfigWith(t *testing.T, f *fakeTama, limitsCfg limits.Limits, mutate func(*profile.Profile)) Config {
	t.Helper()
	return fixtureConfigTuned(t, f, limitsCfg, mutate, nil)
}

func fixtureConfigTuned(
	t *testing.T,
	f *fakeTama,
	limitsCfg limits.Limits,
	mutate func(*profile.Profile),
	tune func(*TaskConfig),
) Config {
	t.Helper()
	return fixtureConfigKind(t, f, limitsCfg, profile.KindSystem, mutate, tune, t.TempDir()+"/state.db", newMemKeys())
}

func appFixtureConfigWith(t *testing.T, f *fakeTama, limitsCfg limits.Limits, mutate func(*profile.Profile)) Config {
	t.Helper()
	return fixtureConfigKind(t, f, limitsCfg, profile.KindApp, mutate, nil, t.TempDir()+"/state.db", newMemKeys())
}

func appFixtureConfigTuned(
	t *testing.T,
	f *fakeTama,
	limitsCfg limits.Limits,
	mutate func(*profile.Profile),
	tune func(*TaskConfig),
) Config {
	t.Helper()
	return fixtureConfigKind(t, f, limitsCfg, profile.KindApp, mutate, tune, t.TempDir()+"/state.db", newMemKeys())
}

func appFixtureConfigAt(
	t *testing.T,
	f *fakeTama,
	limitsCfg limits.Limits,
	mutate func(*profile.Profile),
	tune func(*TaskConfig),
	dbPath string,
	keys store.KeyProvider,
) Config {
	t.Helper()
	return fixtureConfigKind(t, f, limitsCfg, profile.KindApp, mutate, tune, dbPath, keys)
}

func fixtureConfigKind(
	t *testing.T,
	f *fakeTama,
	limitsCfg limits.Limits,
	kind profile.Kind,
	mutate func(*profile.Profile),
	tune func(*TaskConfig),
	dbPath string,
	keys store.KeyProvider,
) Config {
	t.Helper()
	endpoint := f.ts.URL
	p, err := newFixtureProfile(endpoint, kind)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(p)
	}
	endpoint = p.Endpoint
	st, err := store.Open(context.Background(), dbPath, keys, store.Config{Limits: limitsCfg})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

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
	connect := MemoConnect(func(ctx context.Context) (*tama2026.Connection, error) { return adapter.Connect(ctx) })

	cfg := Config{
		Profile:        p,
		Store:          st,
		Connect:        connect,
		AdapterVersion: "test-adapter",
	}
	switch kind {
	case profile.KindApp:
		taskCfg := TaskConfig{
			Owner:         "fixture-tasks",
			LeaseTTL:      30 * time.Second,
			SweepInterval: 50 * time.Millisecond,
		}
		if tune != nil {
			tune(&taskCfg)
		}
		taskService, err := NewTaskService(st, connect, taskCfg)
		if err != nil {
			t.Fatalf("task service: %v", err)
		}
		t.Cleanup(taskService.Stop)
		cfg.Tasks = taskService
	default:
		workerService, err := worker.NewService(st, NewExecutor(connect), worker.Config{
			Owner:    "fixture-worker",
			LeaseTTL: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("worker.NewService: %v", err)
		}
		t.Cleanup(workerService.Stop)
		cfg.Worker = workerService
	}
	return cfg
}

// appFromConfig builds the Service from a caller-customized fixture Config,
// starts the worker, and closes everything with the test.
func appFromConfig(t *testing.T, cfg Config) (*Service, *store.Store, *worker.Service) {
	t.Helper()
	svc, err := New(cfg)
	if err != nil {
		t.Fatalf("application.New: %v", err)
	}
	t.Cleanup(func() { _ = cfg.Store.Close() })
	if cfg.Worker != nil {
		t.Cleanup(cfg.Worker.Stop)
		_ = cfg.Worker.Start(context.Background())
	}
	if cfg.Tasks != nil {
		t.Cleanup(cfg.Tasks.Stop)
		_ = cfg.Tasks.Start(context.Background())
	}
	return svc, cfg.Store, cfg.Worker
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
