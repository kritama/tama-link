package application

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kritama/tama-link/internal/jsonvalue"
)

// taskUpstream is a scripted TamaMCP 2026 endpoint for App-task tests.
type taskUpstream struct {
	ts *httptest.Server

	mu         sync.Mutex
	calls      int
	gets       int
	updates    int
	cancels    int
	graph      int
	auths      int
	failUpdate atomic.Bool
	dropFirst  atomic.Bool
	seenHash   map[string]string
	callLog    []callIdentity
	observed   []authObservation
	subscribes int
	lastCall   string
	lastUpdate string

	// holdCall, when set, blocks tools/call after recording it until closed.
	holdCall chan struct{}
	// holdUpdate, when set, blocks tasks/update after recording it until closed.
	holdUpdate chan struct{}
	// onGet returns the tasks/get JSON result document for the nth get
	// (1-based). A non-200 status is a JSON-RPC error document instead.
	onGet func(n int) (status int, body string)
	// onSubscribe handles subscriptions/listen. Nil means method-not-found,
	// which forces the polling path.
	onSubscribe func(w http.ResponseWriter, r *http.Request, id string)
	// pollIntervalMs overrides the tools/call task handle. Zero uses 20.
	pollIntervalMs int64
}

func newTaskUpstream(t *testing.T) *taskUpstream {
	t.Helper()
	u := &taskUpstream{
		seenHash: map[string]string{},
		onGet: func(int) (int, string) {
			return http.StatusOK, taskState("completed", "2026-09-11T10:00:05Z", taskSuccessResult(false))
		},
	}
	u.ts = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.ts.Close)
	t.Cleanup(func() {
		if u.cancelCount() != 0 {
			t.Errorf("tasks/cancel was invoked %d times", u.cancelCount())
		}
	})
	return u
}

func (u *taskUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var peek struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &peek)
	authorized := r.Header.Get("Authorization") != ""
	u.mu.Lock()
	if authorized {
		u.auths++
	}
	u.observed = append(u.observed, authObservation{Method: peek.Method, Authorized: authorized})
	u.mu.Unlock()
	if peek.Method == "tasks/get" && !authorized {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"unauthorized"}}`)
		return
	}
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &envelope)
	var id string
	_ = json.Unmarshal(envelope.ID, &id)
	quoted := string(envelope.ID)

	switch envelope.Method {
	case "server/discover":
		writeFake(w, quoted, fixtureDiscoverDoc())
	case "tools/list":
		writeFake(w, quoted, fixtureToolsListDoc())
	case "tools/call":
		if conflict := u.noteCall(r, body); conflict {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"identifier reused with different arguments"}}`, quoted)
			return
		}
		u.mu.Lock()
		hold := u.holdCall
		drop := u.dropFirst.CompareAndSwap(true, false)
		u.mu.Unlock()
		if drop {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "cannot drop", http.StatusBadGateway)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
			return
		}
		if hold != nil {
			<-hold
		}
		writeFake(w, quoted, u.taskHandle())
	case "tasks/get":
		u.mu.Lock()
		u.gets++
		n := u.gets
		onGet := u.onGet
		u.mu.Unlock()
		status, doc := http.StatusOK, taskState("working", "2026-09-11T10:00:02Z", "")
		if onGet != nil {
			status, doc = onGet(n)
		}
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":%s}`, quoted, doc)
			return
		}
		writeFake(w, quoted, doc)
	case "tasks/update":
		u.mu.Lock()
		u.updates++
		u.lastUpdate = string(body)
		u.mu.Unlock()
		if u.failUpdate.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		u.mu.Lock()
		hold := u.holdUpdate
		u.mu.Unlock()
		if hold != nil {
			<-hold
		}
		writeFake(w, quoted, `{"resultType":"complete"}`)
	case "tasks/cancel":
		u.mu.Lock()
		u.cancels++
		u.mu.Unlock()
		writeFake(w, quoted, `{"resultType":"complete"}`)
	case "subscriptions/listen":
		u.mu.Lock()
		u.subscribes++
		onSubscribe := u.onSubscribe
		u.mu.Unlock()
		if onSubscribe == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, quoted)
			return
		}
		onSubscribe(w, r, id)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// callIdentity is one tools/call without its JSON-RPC transport id.
type callIdentity struct {
	Tool      string
	Arguments string
	Meta      string
	Protocol  string
	Method    string
}

type authObservation struct {
	Method     string
	Authorized bool
}

// noteCall records one tools/call and reports whether the identifier was
// reused with a different canonical argument document.
func (u *taskUpstream) noteCall(r *http.Request, body []byte) bool {
	identity := projectCall(r, body)
	hash := identity.Tool + "\n" + identity.Arguments
	id := callIdentifier(body)
	if id == "" {
		id = hash
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls++
	u.lastCall = string(body)
	u.callLog = append(u.callLog, identity)
	if prev, ok := u.seenHash[id]; ok && prev != hash {
		return true
	}
	if _, ok := u.seenHash[id]; !ok {
		u.seenHash[id] = hash
		u.graph++
	}
	return false
}

func projectCall(r *http.Request, body []byte) callIdentity {
	var envelope struct {
		Method string `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      json.RawMessage `json:"_meta"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &envelope)
	args, err := jsonvalue.Canonical(envelope.Params.Arguments)
	if err != nil {
		args = envelope.Params.Arguments
	}
	meta, err := jsonvalue.Canonical(envelope.Params.Meta)
	if err != nil {
		meta = envelope.Params.Meta
	}
	return callIdentity{
		Tool:      envelope.Params.Name,
		Arguments: string(args),
		Meta:      string(meta),
		Protocol:  r.Header.Get("MCP-Protocol-Version"),
		Method:    r.Header.Get("Mcp-Method"),
	}
}

func (u *taskUpstream) callIdentities() []callIdentity {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]callIdentity, len(u.callLog))
	copy(out, u.callLog)
	return out
}

func (u *taskUpstream) observations() []authObservation {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]authObservation, len(u.observed))
	copy(out, u.observed)
	return out
}

func TestFixtureRejectsIdentifierWithDifferentArguments(t *testing.T) {
	t.Parallel()

	up := newTaskUpstream(t)
	first := toolsCallBody("req-1", "proc-1", "hello")
	second := toolsCallBody("req-2", "proc-1", "changed")
	postCall(t, up.ts.URL+"/mcp/app", first, http.StatusOK)
	postCall(t, up.ts.URL+"/mcp/app", second, http.StatusConflict)
	if up.graphCount() != 1 {
		t.Fatalf("graph = %d, want 1", up.graphCount())
	}
}

func toolsCallBody(id, identifier, message string) string {
	return `{"jsonrpc":"2.0","id":"` + id + `","method":"tools/call","params":{"name":"message","arguments":{"identifier":"` + identifier + `","message":"` + message + `"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
}

func postCall(t *testing.T, url, body string, want int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "message")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d body %s", resp.StatusCode, want, raw)
	}
}

func callIdentifier(body []byte) string {
	var envelope struct {
		Params struct {
			Arguments struct {
				Identifier string `json:"identifier"`
			} `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &envelope)
	return envelope.Params.Arguments.Identifier
}

func (u *taskUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *taskUpstream) graphCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.graph
}

func (u *taskUpstream) authCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.auths
}

func (u *taskUpstream) getCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.gets
}

func (u *taskUpstream) updateCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.updates
}

func (u *taskUpstream) cancelCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cancels
}

func (u *taskUpstream) subscribeCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.subscribes
}

func (u *taskUpstream) callBody() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastCall
}

func (u *taskUpstream) updateBody() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastUpdate
}

func (u *taskUpstream) taskHandle() string {
	return fmt.Sprintf(`{"resultType":"task","taskId":"task-1","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:01Z","ttlMs":60000,"pollIntervalMs":%d}`, u.pollMS())
}

func (u *taskUpstream) pollMS() int64 {
	if u.pollIntervalMs > 0 {
		return u.pollIntervalMs
	}
	return 20
}

func taskState(status, updated, extra string) string {
	doc := fmt.Sprintf(`{"resultType":"complete","taskId":"task-1","status":%q,"createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":%q,"ttlMs":60000,"pollIntervalMs":20`, status, updated)
	if extra != "" {
		doc += "," + extra
	}
	return doc + "}"
}

func taskSuccessResult(isError bool) string {
	return fmt.Sprintf(`"result":{"resultType":"complete","isError":%t,"content":[{"type":"text","text":"sent"}],"structuredContent":{"status":"completed"}}`, isError)
}

func taskInputRequests() string {
	return `"inputRequests":{"approval":{"mode":"elicitation","schema":{"type":"object"}}}`
}
