package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/credential"
	"github.com/kritama/tama-link/internal/profile"
)

// demoProfileForWrite builds the demo profile without test helpers.
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

// credentialBackendAvailable reports whether the platform credential
// backend can complete an availability probe in this environment. Headless
// runners without a secure service (or one that blocks on user interaction)
// cannot run the durable-state serve path.
func credentialBackendAvailable(t *testing.T) bool {
	t.Helper()
	_, err := credential.New("demo")
	return err == nil
}

// TestServeBinaryStdioHandshake proves the Phase 0 exit criterion end to end:
// serve starts the two-tool STDIO MCP server for an existing profile. When
// the environment has no usable credential backend, the same command must
// fail fast and cleanly instead of hanging.
func TestServeBinaryStdioHandshake(t *testing.T) {
	t.Parallel()

	bin := buildBinary(t)

	if !credentialBackendAvailable(t) {
		t.Log("credential backend unavailable; asserting clean failure")
		var stdout, stderr bytes.Buffer
		configDir := t.TempDir()
		if err := writeDemoProfile(configDir); err != nil {
			t.Fatalf("write profile: %v", err)
		}
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
	var stderrBuf bytes.Buffer
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

	type frame struct {
		ID     json.Number     `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}

	type collected struct {
		init   json.RawMessage
		tools  json.RawMessage
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
			}
			if got.init != nil && got.tools != nil {
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
		if tool.Name != "submit" {
			continue
		}
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
		if want := []string{"message"}; !slices.Equal(schema.Properties.Tool.Enum, want) {
			t.Fatalf("submit tool enum = %v, want %v", schema.Properties.Tool.Enum, want)
		}
	}
}
