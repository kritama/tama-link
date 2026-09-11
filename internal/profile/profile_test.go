package profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/catalog"
	"github.com/kritama/tama-link/internal/limits"
)

func testDescriptor(name string) catalog.Descriptor {
	d := catalog.Descriptor{
		Name:        name,
		Title:       "Message",
		Description: "Send one message to Tama.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
		TaskSupport: catalog.TaskSupportRequired,
		Strategy:    catalog.StrategyUpstreamTask,
	}
	digest, err := d.ComputeDigest()
	if err != nil {
		panic(err)
	}
	d.Digest = digest
	return d
}

func validProfile() *Profile {
	return &Profile{
		Version:      SchemaVersion,
		Name:         Name("tama-app"),
		Origin:       "https://tama.example",
		Endpoint:     "https://tama.example/mcp/app",
		Issuer:       "https://auth.example",
		Instructions: "Upstream instructions.",
		Bounds:       Bounds{ProtocolMin: "2025-03-26", ProtocolMax: "2025-11-25"},
		State:        StateRefs{Database: "default", Credentials: "default"},
		Operations:   []catalog.Descriptor{testDescriptor("message")},
	}
}

func writeProfile(t *testing.T, configDir string, p *Profile) {
	t.Helper()

	dir := filepath.Join(configDir, ProfilesDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tama-app.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	writeProfile(t, configDir, validProfile())

	loaded, err := Load(Name("tama-app"), configDir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if loaded.Name != "tama-app" || loaded.Origin != "https://tama.example" {
		t.Fatalf("loaded profile = %+v", loaded)
	}
	if len(loaded.Operations) != 1 || loaded.Operations[0].Name != "message" {
		t.Fatalf("operations = %+v", loaded.Operations)
	}
	if got := loaded.EffectiveLimits(); got != limits.Default() {
		t.Fatalf("unpinned limits = %+v, want defaults", got)
	}
}

func TestStateLocationsRemainProfileIsolated(t *testing.T) {
	t.Parallel()

	alpha := validProfile()
	alpha.Name = Name("alpha")
	beta := validProfile()
	beta.Name = Name("beta")
	root := t.TempDir()
	alphaPath, err := DatabasePath(root, alpha)
	if err != nil {
		t.Fatalf("DatabasePath alpha: %v", err)
	}
	betaPath, err := DatabasePath(root, beta)
	if err != nil {
		t.Fatalf("DatabasePath beta: %v", err)
	}
	if alphaPath == betaPath {
		t.Fatalf("database paths collided: %s", alphaPath)
	}
	if CredentialNamespace(alpha) == CredentialNamespace(beta) {
		t.Fatal("credential namespaces collided")
	}
}

func TestLoadPinnedLimits(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	p := validProfile()
	p.Limits = &limits.Limits{
		ArgumentsBytes:     32 * limits.KiB,
		ArgumentDepth:      4,
		ResponseBytes:      1 * limits.MiB,
		ResultBytes:        1 * limits.MiB,
		EventBytes:         1 * limits.KiB,
		MaxEvents:          16,
		EventsBytes:        64 * limits.KiB,
		AwaitDefault:       5 * time.Second,
		AwaitMax:           10 * time.Second,
		PayloadRetention:   24 * time.Hour,
		TombstoneRetention: 48 * time.Hour,
	}
	writeProfile(t, configDir, p)

	loaded, err := Load(Name("tama-app"), configDir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got := loaded.EffectiveLimits(); got != *p.Limits {
		t.Fatalf("pinned limits = %+v, want %+v", got, *p.Limits)
	}
}

func TestLoadRejectsMissingProfile(t *testing.T) {
	t.Parallel()

	_, err := Load(Name("tama-app"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), `profile "tama-app" not found at`) {
		t.Fatalf("error = %v, want missing-profile error", err)
	}
}

func TestLoadRejectsDirectory(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	path := Path(configDir, Name("tama-app"))
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Name("tama-app"), configDir); err == nil ||
		!strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("error = %v, want directory error", err)
	}
}

func TestLoadRejectsSymlink(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated privileges on Windows")
	}

	configDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configDir, ProfilesDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := Path(configDir, Name("tama-app"))
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Name("tama-app"), configDir); err == nil ||
		!strings.Contains(err.Error(), "is not a regular file") {
		t.Fatalf("error = %v, want symlink rejection", err)
	}
}

func TestLoadRejectsMalformedDocuments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		doc     string
		wantErr string
	}{
		{"trailing data", `{"version":1} {"version":1}`, "trailing data"},
		{"unknown field", `{"version":1,"unexpected":true}`, "unknown field"},
		{"invalid JSON", `{`, "decode profile"},
		{"duplicate profile key", `{"version":1,"version":1}`, "duplicate object key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			configDir := t.TempDir()
			dir := filepath.Join(configDir, ProfilesDirName)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "tama-app.json"), []byte(test.doc), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(Name("tama-app"), configDir)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Profile)
	}{
		{"wrong version", func(p *Profile) { p.Version = 2 }},
		{"name mismatch", func(p *Profile) { p.Name = Name("other") }},
		{"http origin", func(p *Profile) { p.Origin = "http://tama.example" }},
		{"origin with path", func(p *Profile) { p.Origin = "https://tama.example/mcp" }},
		{"origin with credentials", func(p *Profile) { p.Origin = "https://user@tama.example" }},
		{"endpoint host mismatch", func(p *Profile) { p.Endpoint = "https://other.example/mcp/app" }},
		{"endpoint with credentials", func(p *Profile) { p.Endpoint = "https://user@tama.example/mcp/app" }},
		{"endpoint with query", func(p *Profile) { p.Endpoint = "https://tama.example/mcp/app?other=true" }},
		{"endpoint with fragment", func(p *Profile) { p.Endpoint = "https://tama.example/mcp/app#other" }},
		{"issuer not https", func(p *Profile) { p.Issuer = "http://auth.example" }},
		{"issuer with query", func(p *Profile) { p.Issuer = "https://auth.example/issuer?other=true" }},
		{"bounds reversed", func(p *Profile) {
			p.Bounds = Bounds{ProtocolMin: "2025-11-25", ProtocolMax: "2025-03-26"}
		}},
		{"bounds malformed", func(p *Profile) {
			p.Bounds = Bounds{ProtocolMin: "soon", ProtocolMax: "2025-03-26"}
		}},
		{"state reference with slash", func(p *Profile) { p.State.Database = "../other" }},
		{"state reference dot", func(p *Profile) { p.State.Credentials = "." }},
		{"partial limits", func(p *Profile) {
			p.Limits = &limits.Limits{ArgumentsBytes: 32 * limits.KiB}
		}},
		{"limits above ceiling", func(p *Profile) {
			l := limits.Default()
			l.ArgumentsBytes = 5 * limits.MiB
			p.Limits = &l
		}},
		{"tombstone below payload", func(p *Profile) {
			l := limits.Default()
			l.PayloadRetention = 30 * 24 * time.Hour
			l.TombstoneRetention = 14 * 24 * time.Hour
			p.Limits = &l
		}},
		{"no operations", func(p *Profile) { p.Operations = nil }},
		{"duplicate operations", func(p *Profile) {
			p.Operations = append(p.Operations, p.Operations[0])
		}},
		{"stale digest", func(p *Profile) {
			p.Operations[0].Description = "Tampered description."
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			p := validProfile()
			test.mutate(p)
			if err := p.Validate(Name("tama-app")); err == nil {
				t.Fatalf("Validate() = nil, want error for %s", test.name)
			}
		})
	}
}

func TestValidateAcceptsPinnedLimits(t *testing.T) {
	t.Parallel()

	p := validProfile()
	p.Limits = &limits.Limits{
		ArgumentsBytes:     32 * limits.KiB,
		ArgumentDepth:      4,
		ResponseBytes:      1 * limits.MiB,
		ResultBytes:        1 * limits.MiB,
		EventBytes:         1 * limits.KiB,
		MaxEvents:          16,
		EventsBytes:        64 * limits.KiB,
		AwaitDefault:       5 * time.Second,
		AwaitMax:           10 * time.Second,
		PayloadRetention:   24 * time.Hour,
		TombstoneRetention: 48 * time.Hour,
	}
	if err := p.Validate(Name("tama-app")); err != nil {
		t.Fatalf("Validate() error: %v", err)
	}
}

func TestBoundsValid(t *testing.T) {
	t.Parallel()

	if !(Bounds{ProtocolMin: "2025-03-26", ProtocolMax: "2025-11-25"}.Valid()) {
		t.Fatal("ordered bounds rejected")
	}
	if !(Bounds{ProtocolMin: "2025-03-26", ProtocolMax: "2025-03-26"}.Valid()) {
		t.Fatal("equal bounds rejected")
	}
	if (Bounds{ProtocolMin: "2025-11-25", ProtocolMax: "2025-03-26"}.Valid()) {
		t.Fatal("reversed bounds accepted")
	}
	if (Bounds{ProtocolMin: "nope", ProtocolMax: "2025-03-26"}.Valid()) {
		t.Fatal("malformed bounds accepted")
	}
}
