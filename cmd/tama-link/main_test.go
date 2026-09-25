package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kritama/tama-link/internal/profile"
	"github.com/kritama/tama-link/internal/version"
)

func writeProfileFile(t *testing.T, configDir, name string) {
	t.Helper()
	profilesDir := filepath.Join(configDir, "profiles")
	if err := os.MkdirAll(profilesDir, 0o700); err != nil {
		t.Fatalf("create profiles dir: %v", err)
	}
	path := profile.Path(configDir, profile.Name(name))
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}

func TestRunRequiresACommand(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("stderr = %q, want usage", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"bogus"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `unknown command "bogus"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunRejectsPositionalArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{"serve", []string{"serve", "--profile", "demo", "extra"}},
		{"login", []string{"login", "--profile", "demo", "extra"}},
		{"logout", []string{"logout", "--profile", "demo", "extra"}},
		{"version", []string{"version", "junk"}},
		{"help", []string{"help", "extra"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := run(context.Background(), test.args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), "no positional arguments") &&
				!strings.Contains(stderr.String(), "takes no arguments") {
				t.Fatalf("stderr = %q, want positional-argument error", stderr.String())
			}
		})
	}
}

func TestRunHelp(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"help"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("stdout = %q, want usage", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRunVersionHuman(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"version"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if want := version.String() + "\n"; stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunVersionJSON(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"version", "--json"}, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	var doc versionOutput
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	want := versionOutput{
		Name:    "tama-link",
		Version: version.Version,
		Commit:  version.Commit,
		Built:   version.BuildDate,
	}
	if doc != want {
		t.Fatalf("document = %+v, want %+v", doc, want)
	}

	encoded := stdout.Bytes()
	if !bytes.HasSuffix(encoded, []byte("\n")) {
		t.Fatalf("JSON output must end with a newline: %q", encoded)
	}
}

func TestRunServeRequiresProfile(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"serve"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "serve requires --profile") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunServeRejectsInvalidProfileName(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"serve", "--profile", "Bad Name"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid profile name") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunServeFailsWhenProfileIsMissing(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"serve", "--profile", "demo", "--config-dir", t.TempDir(),
	}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `profile "demo" not found at`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunServeFailsWhenProfilePathIsADirectory(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	writeProfileFile(t, configDir, "demo")
	// Replace the file with a directory of the same name.
	dirPath := filepath.Join(configDir, "profiles", "demo.json")
	if err := os.Remove(dirPath); err != nil {
		t.Fatalf("remove profile file: %v", err)
	}
	if err := os.Mkdir(dirPath, 0o700); err != nil {
		t.Fatalf("make directory: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"serve", "--profile", "demo", "--config-dir", configDir,
	}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "is a directory") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunLoginMissingProfile(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"login", "--profile", "demo", "--config-dir", t.TempDir()}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), `profile "demo" not found at`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

// TestRunLoginVersion1ProfileFailsClosed proves the migration contract:
// a version 1 profile loads for the non-interactive runtime but login
// rejects it with an actionable error instead of an implicit scope set.
func TestRunLoginVersion1ProfileFailsClosed(t *testing.T) {
	t.Parallel()

	configDir := t.TempDir()
	p := demoProfileForWrite()
	p.Version = profile.LegacySchemaVersion
	p.Scopes = nil
	if err := p.Validate("demo"); err != nil {
		t.Fatalf("legacy profile: %v", err)
	}
	if err := writeDemoProfileFile(configDir, "demo", p); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"login", "--profile", "demo", "--config-dir", configDir,
	}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "regenerate it as version 2") {
		t.Fatalf("stderr = %q, want the migration error", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunLoginRequiresProfile(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"login"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "login requires --profile") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunLogoutStub(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"logout", "--profile", "demo"}, &stdout, &stderr)

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "logout is not implemented in this phase") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunLogoutRejectsInvalidProfileName(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"logout", "--profile", "UPPER"}, &stdout, &stderr)

	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid profile name") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
