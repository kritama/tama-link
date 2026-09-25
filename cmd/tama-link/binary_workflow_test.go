package main

import (
	"bufio"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/profile"
)

const binaryInstructions = "Binary fixture instructions."

func TestBinaryStdioCompletesAppAndSystemWorkflows(t *testing.T) {
	fx := newBinaryFixture()
	ts := httptest.NewTLSServer(http.HandlerFunc(fx.serve))
	t.Cleanup(ts.Close)
	caPath := writeFixtureCA(t, ts.Certificate())
	bin := buildFixtureBinary(t)
	configDir := t.TempDir()
	writeBinaryProfile(t, configDir, "tama-app", ts.URL, "/mcp/app", appOperation())
	writeBinaryProfile(t, configDir, "tama-system", ts.URL, "/mcp/system", systemOperation())

	t.Run("app", func(t *testing.T) {
		session := startBinaryServe(t, bin, configDir, "tama-app", caPath)
		session.initialize()
		assertBinaryTools(t, session.tools())
		submitted := session.call("submit", map[string]any{
			"tool":              "message",
			"client_request_id": "app-1",
			"arguments":         map[string]any{"message": "secret-argument-marker"},
		})
		if code := toolCode(t, submitted); code != "" {
			t.Fatalf("submit = %s", submitted)
		}
		id := toolString(t, submitted, "submission_id")
		var pending map[string]any
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			pending = session.call("await", map[string]any{"submission_id": id, "timeout_ms": 200})
			if toolString(t, pending, "status") == "input_required" {
				break
			}
		}
		if toolString(t, pending, "status") != "input_required" {
			t.Fatalf("await = %s", pendingJSON(pending))
		}
		answered := session.call("await", map[string]any{
			"submission_id":   id,
			"timeout_ms":      200,
			"input_responses": map[string]any{"approval": map[string]any{"action": "accept"}},
		})
		if code := toolCode(t, answered); code != "" {
			t.Fatalf("input response = %s", pendingJSON(answered))
		}
		var terminal map[string]any
		for time.Now().Before(deadline) {
			terminal = session.call("await", map[string]any{"submission_id": id, "timeout_ms": 500})
			if toolBool(terminal, "terminal") {
				break
			}
		}
		if !toolBool(terminal, "terminal") || toolString(t, terminal, "status") != "completed" {
			t.Fatalf("terminal = %s", pendingJSON(terminal))
		}
		cursor := toolString(t, terminal, "cursor")
		retained := session.call("await", map[string]any{"submission_id": id, "timeout_ms": 1, "cursor": cursor})
		if !toolBool(retained, "terminal") || toolString(t, retained, "status") != "completed" {
			t.Fatalf("retained = %s", pendingJSON(retained))
		}
		if strings.Contains(session.stderr.String(), "secret-argument-marker") {
			t.Fatalf("stderr leaked the argument: %s", session.stderr.String())
		}
		if fx.callCount("message") != 1 {
			t.Fatalf("app tools/call = %d, want 1", fx.callCount("message"))
		}
	})

	t.Run("system", func(t *testing.T) {
		session := startBinaryServe(t, bin, configDir, "tama-system", caPath)
		session.initialize()
		submitted := session.call("submit", map[string]any{
			"tool":              "status",
			"client_request_id": "sys-1",
			"arguments":         map[string]any{"detail": "unit"},
			"client_context":    map[string]any{"thread_id": "thread-1"},
		})
		if code := toolCode(t, submitted); code != "" {
			t.Fatalf("submit = %s", pendingJSON(submitted))
		}
		id := toolString(t, submitted, "submission_id")
		replay := session.call("submit", map[string]any{
			"tool":              "status",
			"client_request_id": "sys-1",
			"arguments":         map[string]any{"detail": "unit"},
			"client_context":    map[string]any{"thread_id": "thread-1"},
		})
		if toolString(t, replay, "submission_id") != id {
			t.Fatalf("replay id = %s, want %s", toolString(t, replay, "submission_id"), id)
		}
		conflict := session.call("submit", map[string]any{
			"tool":              "status",
			"client_request_id": "sys-1",
			"arguments":         map[string]any{"detail": "changed"},
			"client_context":    map[string]any{"thread_id": "thread-1"},
		})
		if toolCode(t, conflict) != "idempotency_conflict" {
			t.Fatalf("conflict = %s", pendingJSON(conflict))
		}
		var terminal map[string]any
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			terminal = session.call("await", map[string]any{"submission_id": id, "timeout_ms": 500})
			if toolBool(terminal, "terminal") {
				break
			}
		}
		if !toolBool(terminal, "terminal") || toolString(t, terminal, "status") != "completed" {
			t.Fatalf("terminal = %s", pendingJSON(terminal))
		}
		cursor := toolString(t, terminal, "cursor")
		retained := session.call("await", map[string]any{"submission_id": id, "timeout_ms": 1, "cursor": cursor})
		if !toolBool(retained, "terminal") || toolString(t, retained, "status") != "completed" {
			t.Fatalf("retained = %s", pendingJSON(retained))
		}
		if fx.callCount("status") != 1 {
			t.Fatalf("system tools/call = %d, want 1", fx.callCount("status"))
		}
	})
}

type binaryFixture struct {
	mu      sync.Mutex
	calls   map[string]int
	updated bool
}

func newBinaryFixture() *binaryFixture {
	return &binaryFixture{calls: map[string]int{}}
}

func (f *binaryFixture) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *binaryFixture) serve(w http.ResponseWriter, r *http.Request) {
	var envelope struct {
		ID     string          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&envelope)
	quoted := `"` + envelope.ID + `"`
	switch envelope.Method {
	case "server/discover":
		writeBinary(w, quoted, `{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{},"extensions":{"io.modelcontextprotocol/tasks":{}}},"instructions":`+jsonString(binaryInstructions)+`,"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"tama","version":"1.0.0"}}}`)
	case "tools/list":
		writeBinary(w, quoted, `{"resultType":"complete","tools":[`+binaryTools()+`]}`)
	case "tools/call":
		var params struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(envelope.Params, &params)
		f.mu.Lock()
		f.calls[params.Name]++
		f.mu.Unlock()
		if params.Name == "status" {
			writeBinary(w, quoted, `{"resultType":"complete","isError":false,"content":[{"type":"text","text":"status ok"}],"structuredContent":{"ok":true}}`)
			return
		}
		writeBinary(w, quoted, `{"resultType":"task","taskId":"task-1","status":"working","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:01Z","ttlMs":60000,"pollIntervalMs":20}`)
	case "tasks/get":
		f.mu.Lock()
		updated := f.updated
		f.mu.Unlock()
		if !updated {
			writeBinary(w, quoted, `{"resultType":"complete","taskId":"task-1","status":"input_required","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:04Z","ttlMs":60000,"pollIntervalMs":20,"inputRequests":{"approval":{"mode":"elicitation","schema":{"type":"object"}}}}`)
			return
		}
		writeBinary(w, quoted, `{"resultType":"complete","taskId":"task-1","status":"completed","createdAt":"2026-09-11T10:00:00Z","lastUpdatedAt":"2026-09-11T10:00:08Z","ttlMs":60000,"pollIntervalMs":20,"result":{"resultType":"complete","isError":false,"content":[{"type":"text","text":"sent"}],"structuredContent":{"status":"completed"}}}`)
	case "tasks/update":
		f.mu.Lock()
		f.updated = true
		f.mu.Unlock()
		writeBinary(w, quoted, `{"resultType":"complete"}`)
	default:
		http.Error(w, "unexpected method", http.StatusNotFound)
	}
}

func writeBinary(w http.ResponseWriter, id, doc string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, id, doc)
}

func binaryTools() string {
	return `{"name":"status","description":"Read status","inputSchema":{"type":"object","properties":{"detail":{"type":"string"}},"required":["detail"]},"outputSchema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}},` +
		`{"name":"message","description":"Send a message","inputSchema":{"type":"object","properties":{"message":{"type":"string","minLength":1}},"required":["message"]},"outputSchema":{"type":"object","properties":{"status":{"type":"string"}},"required":["status"]}}`
}

func appOperation() catalog.Descriptor {
	d := catalog.Descriptor{
		Name:         "message",
		Description:  "Send a message",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"message":{"type":"string","minLength":1}},"required":["message"]}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string"}},"required":["status"]}`),
		TaskSupport:  catalog.TaskSupportRequired,
		Bindings:     []catalog.Binding{{Source: catalog.SourceClientRequestID, Target: "/identifier", Required: true}},
		Strategy:     catalog.StrategyUpstreamTask,
	}
	digest, err := d.ComputeDigest()
	if err != nil {
		panic(err)
	}
	d.Digest = digest
	return d
}

func systemOperation() catalog.Descriptor {
	d := catalog.Descriptor{
		Name:         "status",
		Description:  "Read status",
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"detail":{"type":"string"}},"required":["detail"]}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
		TaskSupport:  catalog.TaskSupportForbidden,
		Bindings: []catalog.Binding{
			{Source: catalog.SourceClientRequestID, Target: "/client_meta/client_request_id", Required: true},
			{Source: catalog.SourceClientContextThreadID, Target: "/client_meta/thread_id", Required: false},
		},
		Strategy: catalog.StrategyLocalReplayable,
	}
	digest, err := d.ComputeDigest()
	if err != nil {
		panic(err)
	}
	d.Digest = digest
	return d
}

func writeBinaryProfile(t *testing.T, configDir, name, base, path string, op catalog.Descriptor) {
	t.Helper()
	p := profile.Profile{
		Version:      profile.SchemaVersion,
		Name:         profile.Name(name),
		Origin:       base,
		Endpoint:     base + path,
		Issuer:       base,
		Instructions: binaryInstructions,
		Bounds:       profile.Bounds{ProtocolMin: "2026-07-28", ProtocolMax: "2026-07-28"},
		State:        profile.StateRefs{Database: "default", Credentials: "default"},
		Operations:   []catalog.Descriptor{op},
		Scopes:       []string{"mcp.message"},
	}
	if err := p.Validate(profile.Name(name)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(configDir, "profiles")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeFixtureCA(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	if cert == nil {
		t.Fatal("fixture server has no certificate")
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func buildFixtureBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "tama-link")
	cmd := exec.Command("go", "build", "-tags", "tamalinkfixture", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture binary: %v\n%s", err, out)
	}
	return bin
}

type binarySession struct {
	t          *testing.T
	sendWriter io.WriteCloser
	nextID     int
	stderr     *syncBuffer
	read       chan binaryFrame
	done       chan struct{}
}

type binaryFrame struct {
	ID     json.Number     `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func startBinaryServe(t *testing.T, bin, configDir, name, caPath string) *binarySession {
	t.Helper()
	cmd := exec.Command(bin, "serve", "--profile", name, "--config-dir", configDir)
	cmd.Env = append(os.Environ(), "TAMA_LINK_FIXTURE=1", "TAMA_LINK_FIXTURE_CA="+caPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	session := &binarySession{t: t, nextID: 1, stderr: stderr, read: make(chan binaryFrame, 8), done: done}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var frame binaryFrame
			if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
				return
			}
			select {
			case session.read <- frame:
			case <-done:
				return
			}
		}
	}()
	// stdin is an io.WriteCloser, not *os.File. Store it through a small wrapper.
	session.sendWriter = stdin
	return session
}

func (s *binarySession) initialize() {
	s.t.Helper()
	id := s.nextID
	s.nextID++
	s.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "binary-workflow", "version": "0"},
		},
	})
	_ = s.waitFor(id)
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
}

func (s *binarySession) tools() map[string]any {
	s.t.Helper()
	return s.callRaw("tools/list", map[string]any{})
}

func (s *binarySession) call(name string, arguments map[string]any) map[string]any {
	s.t.Helper()
	return s.callRaw("tools/call", map[string]any{"name": name, "arguments": arguments})
}

func (s *binarySession) callRaw(method string, params any) map[string]any {
	s.t.Helper()
	id := s.nextID
	s.nextID++
	s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	frame := s.waitFor(id)
	if len(frame.Error) > 0 {
		s.t.Fatalf("%s error: %s stderr=%s", method, frame.Error, s.stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal(frame.Result, &result); err != nil {
		s.t.Fatalf("decode %s: %v", method, err)
	}
	return result
}

func (s *binarySession) send(value any) {
	s.t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.sendWriter.Write(append(encoded, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

func (s *binarySession) wait() binaryFrame {
	s.t.Helper()
	select {
	case frame := <-s.read:
		return frame
	case <-s.done:
		s.t.Fatal("server stopped while waiting for a frame")
		return binaryFrame{}
	case <-time.After(15 * time.Second):
		s.t.Fatalf("timed out waiting for a frame; stderr=%s", s.stderr.String())
		return binaryFrame{}
	}
}

func (s *binarySession) waitFor(id int) binaryFrame {
	s.t.Helper()
	want := strconv.Itoa(id)
	for {
		frame := s.wait()
		if frame.ID.String() == want {
			return frame
		}
	}
}

func assertBinaryTools(t *testing.T, result map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(result)
	if !strings.Contains(string(raw), `"submit"`) || !strings.Contains(string(raw), `"await"`) || !strings.Contains(string(raw), `"input_responses"`) {
		t.Fatalf("tools = %s", raw)
	}
}

func toolString(t *testing.T, result map[string]any, field string) string {
	t.Helper()
	content, _ := result["structuredContent"].(map[string]any)
	value, _ := content[field].(string)
	if value == "" {
		t.Fatalf("missing %s in %s", field, pendingJSON(result))
	}
	return value
}

func toolBool(result map[string]any, field string) bool {
	content, _ := result["structuredContent"].(map[string]any)
	value, _ := content[field].(bool)
	return value
}

func toolCode(t *testing.T, result map[string]any) string {
	t.Helper()
	if isError, _ := result["isError"].(bool); !isError {
		return ""
	}
	content, _ := result["structuredContent"].(map[string]any)
	errObj, _ := content["error"].(map[string]any)
	code, _ := errObj["code"].(string)
	return code
}

func pendingJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
