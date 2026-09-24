package main

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/99designs/keyring"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/credential"
	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/server"
	"github.com/kritama/tama-link/internal/version"
)

// TestServeStackSpeaksStdioWithoutPlatformKeyring drives the production
// server over the same newline-delimited STDIO framing the binary uses.
// The platform keyring is not required: the credential backend is injected
// at the existing buildApp seam, which production still resolves through
// credential.New.
func TestServeStackSpeaksStdioWithoutPlatformKeyring(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	if err := writeDemoProfile(configDir); err != nil {
		t.Fatal(err)
	}
	p, err := profile.Load(profile.Name("demo"), configDir)
	if err != nil {
		t.Fatal(err)
	}
	app, cleanup, err := buildApp(context.Background(), p, configDir, func(namespace string) (*credential.Keyring, error) {
		return credential.NewWithBackend(namespace, newMemoryKeyring()), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	t.Cleanup(func() {
		_ = clientWriter.Close()
		_ = serverWriter.Close()
	})

	srv := server.New(p, version.Version, app)
	runErr := make(chan error, 1)
	go func() {
		runErr <- srv.Run(ctx, &mcp.IOTransport{Reader: serverReader, Writer: serverWriter})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: clientReader, Writer: clientWriter}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	if !strings.Contains(session.InitializeResult().Instructions, "submit") {
		t.Fatalf("instructions = %q", session.InitializeResult().Instructions)
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		if tool.Name != contract.ToolAwait {
			continue
		}
		schema, _ := tool.InputSchema.(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		if _, ok := properties["input_responses"]; !ok {
			t.Fatalf("await schema = %v", schema)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{contract.ToolAwait, contract.ToolSubmit}) {
		t.Fatalf("tools = %v", names)
	}

	const marker = "secret-argument-marker"
	submit, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      contract.ToolSubmit,
		Arguments: json.RawMessage(`{"tool":"message","arguments":{"message":"` + marker + `"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(submit)
	if !submit.IsError || !strings.Contains(string(encoded), string(contract.CodeAuthenticationRequired)) {
		t.Fatalf("submit = %s", encoded)
	}
	if strings.Contains(string(encoded), marker) || strings.Contains(string(encoded), "not_implemented") {
		t.Fatalf("submit leaked the argument or stayed unimplemented: %s", encoded)
	}

	await, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      contract.ToolAwait,
		Arguments: json.RawMessage(`{"submission_id":"missing"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitEncoded, _ := json.Marshal(await)
	if !await.IsError || !strings.Contains(string(awaitEncoded), string(contract.CodeSubmissionNotFound)) {
		t.Fatalf("await = %s", awaitEncoded)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil && ctx.Err() == nil {
			t.Fatalf("server run: %v", err)
		}
	default:
	}
}

type memoryKeyring struct {
	mu    sync.Mutex
	items map[string]keyring.Item
}

func newMemoryKeyring() *memoryKeyring {
	return &memoryKeyring{items: map[string]keyring.Item{}}
}

func (m *memoryKeyring) Get(key string) (keyring.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.items[key]
	if !ok {
		return keyring.Item{}, keyring.ErrKeyNotFound
	}
	return item, nil
}

func (m *memoryKeyring) GetMetadata(string) (keyring.Metadata, error) {
	return keyring.Metadata{}, keyring.ErrMetadataNotSupported
}

func (m *memoryKeyring) Set(item keyring.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[item.Key] = item
	return nil
}

func (m *memoryKeyring) Remove(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, key)
	return nil
}

func (m *memoryKeyring) Keys() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.items))
	for key := range m.items {
		keys = append(keys, key)
	}
	return keys, nil
}
