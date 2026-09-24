package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/profile"
)

// demoProfileForWrite builds the demo profile without test helpers.
// syncBuffer is a stderr sink that child processes and the test can use
// at the same time. os/exec writes from its own goroutine until Wait.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func demoProfileForWrite() profile.Profile {
	op := catalog.Descriptor{
		Name:        "message",
		Title:       "Message",
		Description: "Send one message to Tama.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
		TaskSupport: catalog.TaskSupportRequired,
		Strategy:    catalog.StrategyUpstreamTask,
	}
	digest, err := op.ComputeDigest()
	if err != nil {
		panic(err)
	}
	op.Digest = digest

	p := profile.Profile{
		Version:      profile.SchemaVersion,
		Name:         "demo",
		Origin:       "https://tama.example",
		Endpoint:     "https://tama.example/mcp/app",
		Issuer:       "https://auth.example",
		Instructions: "Pinned upstream instructions.",
		Bounds:       profile.Bounds{ProtocolMin: "2026-07-28", ProtocolMax: "2026-07-28"},
		State:        profile.StateRefs{Database: "default", Credentials: "default"},
		Operations:   []catalog.Descriptor{op},
	}
	if err := p.Validate("demo"); err != nil {
		panic(err)
	}
	return p
}

// writeDemoProfile installs the demo profile under configDir.
func writeDemoProfile(configDir string) error {
	profilesDir := filepath.Join(configDir, "profiles")
	if err := os.MkdirAll(profilesDir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(demoProfileForWrite())
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(profilesDir, "demo.json"), data, 0o600)
}

// buildBinary compiles the real command so the tests exercise the same
// binary a client would launch.
func buildBinary(t *testing.T) string {
	t.Helper()

	bin := filepath.Join(t.TempDir(), "tama-link")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build tama-link: %v\n%s", err, out)
	}
	return bin
}

func TestServeBinaryFailsCleanlyWithoutProfile(t *testing.T) {
	t.Parallel()

	bin := buildBinary(t)
	var stdout, stderr bytes.Buffer

	cmd := exec.Command(bin, "serve", "--profile", "demo", "--config-dir", t.TempDir())
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("exit error = %v, want exit code 2", err)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("exit code = %d, want 2", exitErr.ExitCode())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), `profile "demo" not found at`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// serveCredentialFailure runs serve with no reachable credential backend
// and asserts the fail-closed contract: fast exit 2, empty stdout, and the
// stable unavailable message on stderr.
func serveCredentialFailure(t *testing.T, bin string) {
	t.Helper()
	configDir := t.TempDir()
	if err := writeDemoProfile(configDir); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "serve", "--profile", "demo", "--config-dir", configDir)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 2 {
			t.Fatalf("exit = %v, want code 2", err)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("serve did not fail fast without a credential backend")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "credential backend unavailable") {
		t.Fatalf("stderr = %q, want credential backend unavailable", stderr.String())
	}
}

// e2eKeyringEnv opts the handshake test into the real platform keyring.
// Unset, the test is hermetic and headless-safe: the spawned serve process
// starts with no D-Bus session behind it, so the keyring library registers
// no secure backend at its init and the test can never trigger an
// interactive unlock prompt.
const e2eKeyringEnv = "TAMA_LINK_E2E_KEYRING"

// TestServeBinaryStdioHandshake proves the Phase 0 exit criterion end to end:
// serve starts the two-tool STDIO MCP server for an existing profile. When
// the environment has no usable credential backend, the same command must
// fail fast and cleanly instead of hanging.
//
// The branch is chosen by TAMA_LINK_E2E_KEYRING, never by probing the test
// process's own keyring: the keyring library registers its backends in init
// from the live environment and caches the session bus, so an in-process
// probe would answer for the desktop, not for the headless serve process.
// Unset (the CI default), the spawned serve sees a dead D-Bus socket and
// must fail cleanly. Set to 1, the test runs the full handshake against the
// real platform keyring; an interactive unlock prompt may appear and must
// be answered within the probe bound.
func TestServeBinaryStdioHandshake(t *testing.T) {
	// Not parallel: the test pins environment for the spawned serve
	// process.
	hermetic := os.Getenv(e2eKeyringEnv) == ""
	if hermetic {
		// The dead-bus environment only constrains the SecretService
		// backend: darwin selects Keychain and windows selects the
		// Windows credential store, where the pinned address changes
		// nothing and the spawned serve can complete its probe. Skip the
		// platforms the hermetic branch cannot control instead of failing
		// its exit-code assertions.
		switch runtime.GOOS {
		case "linux":
			t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(t.TempDir(), "no-such-bus"))
		default:
			t.Skipf("hermetic credential failure is only controllable on linux (D-Bus); %s selects a different platform backend", runtime.GOOS)
		}
	}
	bin := buildBinary(t)

	if hermetic {
		serveCredentialFailure(t, bin)
		return
	}

	configDir := t.TempDir()
	if err := writeDemoProfile(configDir); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	cmd := exec.Command(bin, "serve", "--profile", "demo", "--config-dir", configDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderrBuf syncBuffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	send := func(value any) {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal frame: %v", err)
		}
		if _, err := stdin.Write(append(encoded, '\n')); err != nil {
			t.Fatalf("write frame: %v", err)
		}
	}

	send(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "tama-link-e2e", "version": "0"},
		},
	})
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}})
	send(map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "submit",
			"arguments": map[string]any{
				"tool":      "message",
				"arguments": map[string]any{"message": "secret-argument-marker"},
			},
		},
	})
	send(map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "await",
			"arguments": map[string]any{"submission_id": "missing"},
		},
	})

	type frame struct {
		ID     json.Number     `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}

	type collected struct {
		init   json.RawMessage
		tools  json.RawMessage
		submit json.RawMessage
		await  json.RawMessage
		failed string
	}
	result := make(chan collected, 1)
	go func() {
		got := collected{}
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			var f frame
			if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
				got.failed = fmt.Sprintf("invalid frame %q: %v", scanner.Text(), err)
				return
			}
			if len(f.Error) > 0 {
				got.failed = string(f.Error)
				return
			}
			switch f.ID.String() {
			case "1":
				got.init = f.Result
			case "2":
				got.tools = f.Result
			case "3":
				got.submit = f.Result
			case "4":
				got.await = f.Result
			}
			if got.init != nil && got.tools != nil && got.submit != nil && got.await != nil {
				result <- got
				return
			}
		}
		got.failed = fmt.Sprintf("server closed stdout: %q", stderrBuf.String())
		result <- got
	}()

	var got collected
	select {
	case got = <-result:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for MCP responses; stderr=%q", stderrBuf.String())
	}
	if got.failed != "" {
		t.Fatalf("handshake failed: %s", got.failed)
	}

	var initResult struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(got.init, &initResult); err != nil {
		t.Fatalf("unmarshal initialize result: %v", err)
	}
	if initResult.ServerInfo.Name != "tama-link" {
		t.Fatalf("serverInfo.name = %q, want tama-link", initResult.ServerInfo.Name)
	}
	if initResult.ProtocolVersion != "2025-03-26" {
		t.Fatalf("protocolVersion = %q, want 2025-03-26", initResult.ProtocolVersion)
	}
	if !strings.Contains(initResult.Instructions, "submit") ||
		!strings.Contains(initResult.Instructions, "Pinned upstream instructions.") {
		t.Fatalf("instructions = %q, want composed workflow and pinned copy", initResult.Instructions)
	}

	var toolsResult struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(got.tools, &toolsResult); err != nil {
		t.Fatalf("unmarshal tools/list result: %v", err)
	}

	names := make([]string, 0, len(toolsResult.Tools))
	for _, tool := range toolsResult.Tools {
		names = append(names, tool.Name)
		if len(tool.InputSchema) == 0 {
			t.Fatalf("tool %q has no input schema", tool.Name)
		}
	}
	slices.Sort(names)
	if want := []string{"await", "submit"}; !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	for _, tool := range toolsResult.Tools {
		switch tool.Name {
		case "submit":
			var schema struct {
				Properties struct {
					Tool struct {
						Enum []string `json:"enum"`
					} `json:"tool"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				t.Fatalf("unmarshal submit input schema: %v", err)
			}
			if len(schema.Properties.Tool.Enum) != 0 {
				t.Fatalf("submit tool enum = %v, want unconstrained", schema.Properties.Tool.Enum)
			}
		case "await":
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			}
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				t.Fatalf("unmarshal await input schema: %v", err)
			}
			if _, ok := schema.Properties["input_responses"]; !ok {
				t.Fatal("await schema missing input_responses")
			}
			if !slices.Equal(schema.Required, []string{"submission_id"}) {
				t.Fatalf("await required = %v", schema.Required)
			}
		}
	}
	if !strings.Contains(string(got.submit), "authentication_required") {
		t.Fatalf("submit = %s, want authentication_required from the wired handler", got.submit)
	}
	if strings.Contains(string(got.submit), "not_implemented") || strings.Contains(stderrBuf.String(), "secret-argument-marker") {
		t.Fatalf("submit leaked implementation state or arguments: result %s stderr %q", got.submit, stderrBuf.String())
	}
	if !strings.Contains(string(got.await), "submission_not_found") {
		t.Fatalf("await = %s, want submission_not_found", got.await)
	}
}
