package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	keyring "github.com/99designs/keyring"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/contract"
	"github.com/kritama/tama-link/internal/credential"
	"github.com/kritama/tama-link/internal/profile"
)

// memKeyring is an in-memory keyring.Keyring shared by every profile in the
// wiring test, standing in for the platform credential store.
type memKeyring struct {
	mu    sync.Mutex
	items map[string]keyring.Item
}

func newMemKeyring() *memKeyring { return &memKeyring{items: map[string]keyring.Item{}} }

func (m *memKeyring) Get(key string) (keyring.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[key]
	if !ok {
		return keyring.Item{}, keyring.ErrKeyNotFound
	}
	return it, nil
}

func (m *memKeyring) GetMetadata(string) (keyring.Metadata, error) {
	return keyring.Metadata{}, keyring.ErrMetadataNotSupported
}

func (m *memKeyring) Set(item keyring.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[item.Key] = item
	return nil
}

func (m *memKeyring) Remove(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, key)
	return nil
}

func (m *memKeyring) Reset() error { return nil }

func (m *memKeyring) Keys() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.items))
	for key := range m.items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// wiringProfile builds one validated profile. Every profile in the wiring
// test uses the same database and credential references so only the profile
// name can separate them.
func wiringProfile(t *testing.T, name, origin, tool string) *profile.Profile {
	t.Helper()
	d := catalog.Descriptor{
		Name:         tool,
		Description:  "Wiring fixture " + tool,
		InputSchema:  json.RawMessage(`{"type":"object","properties":{"note":{"type":"string"}}}`),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
		TaskSupport:  catalog.TaskSupportForbidden,
		Strategy:     catalog.StrategyLocalReplayable,
	}
	digest, err := d.ComputeDigest()
	if err != nil {
		t.Fatalf("digest %s: %v", tool, err)
	}
	d.Digest = digest

	p := &profile.Profile{
		Version:      profile.SchemaVersion,
		Name:         profile.Name(name),
		Origin:       origin,
		Endpoint:     origin + "/mcp/system",
		Issuer:       "https://issuer.example",
		Instructions: "Wiring instructions for " + name,
		Bounds:       profile.Bounds{ProtocolMin: "2026-07-28", ProtocolMax: "2026-07-28"},
		State:        profile.StateRefs{Database: "default", Credentials: "default"},
		Operations:   []catalog.Descriptor{d},
	}
	if err := p.Validate(profile.Name(name)); err != nil {
		t.Fatalf("validate %s: %v", name, err)
	}
	return p
}

// TestBuildAppKeepsProfilesIsolated drives the production buildApp wiring
// for two named profiles whose database and credential references are both
// "default". The wiring must resolve canonical, profile-scoped locations so
// the two runtimes coexist with distinct databases, credential namespaces,
// and catalogs.
func TestBuildAppKeepsProfilesIsolated(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	backend := newMemKeyring()
	open := func(namespace string) (*credential.Keyring, error) {
		return credential.NewWithBackend(namespace, backend), nil
	}

	alpha := wiringProfile(t, "alpha", "https://alpha.example", "alpha.tool")
	beta := wiringProfile(t, "beta", "https://beta.example", "beta.tool")

	appA, cleanupA, err := buildApp(context.Background(), alpha, configDir, serveHooks{open: open})
	if err != nil {
		t.Fatalf("buildApp alpha: %v", err)
	}
	defer cleanupA()
	appB, cleanupB, err := buildApp(context.Background(), beta, configDir, serveHooks{open: open})
	if err != nil {
		t.Fatalf("buildApp beta: %v", err)
	}
	defer cleanupB()

	// Distinct canonical databases, each under its own profile directory.
	pathA, nsA, err := stateLayout(alpha, configDir)
	if err != nil {
		t.Fatalf("stateLayout alpha: %v", err)
	}
	pathB, nsB, err := stateLayout(beta, configDir)
	if err != nil {
		t.Fatalf("stateLayout beta: %v", err)
	}
	if pathA == pathB {
		t.Fatalf("both profiles resolved the same database %s", pathA)
	}
	wantA := filepath.Join(configDir, "profiles", "alpha", "default.db")
	if pathA != wantA {
		t.Fatalf("alpha database = %s, want the canonical %s", pathA, wantA)
	}
	for _, path := range []string{pathA, pathB} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("database %s was not created: %v", path, err)
		}
	}
	if nsA == nsB {
		t.Fatalf("both profiles resolved the same credential namespace %s", nsA)
	}
	if nsA != "alpha/default" || nsB != "beta/default" {
		t.Fatalf("namespaces = %q, %q; want alpha/default and beta/default", nsA, nsB)
	}

	// The state-encryption keys live in distinct credential spaces: no key
	// written by one profile's wiring may appear under the other's prefix.
	keys, _ := backend.Keys()
	seenA, seenB := false, false
	for _, key := range keys {
		switch {
		case len(key) > len(nsA+"/") && key[:len(nsA)+1] == nsA+"/":
			seenA = true
		case len(key) > len(nsB+"/") && key[:len(nsB)+1] == nsB+"/":
			seenB = true
		default:
			t.Fatalf("credential entry %q is outside both profile namespaces", key)
		}
	}
	if !seenA || !seenB {
		t.Fatalf("credential entries %v do not cover both namespaces", keys)
	}

	// Catalogs stay isolated: each runtime rejects the other profile's tool.
	if _, appErr := appA.Submit(context.Background(), contract.SubmitInput{
		Tool: "beta.tool", ClientRequestID: "cross-a", Arguments: json.RawMessage(`{}`),
	}); appErr == nil || appErr.Code != contract.CodeOperationNotAllowed {
		t.Fatalf("alpha submit beta.tool = %+v, want operation_not_allowed", appErr)
	}
	if _, appErr := appB.Submit(context.Background(), contract.SubmitInput{
		Tool: "alpha.tool", ClientRequestID: "cross-b", Arguments: json.RawMessage(`{}`),
	}); appErr == nil || appErr.Code != contract.CodeOperationNotAllowed {
		t.Fatalf("beta submit alpha.tool = %+v, want operation_not_allowed", appErr)
	}
}
