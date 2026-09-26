//go:build linux || darwin

package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPrivateFileRejectsPermissiveFile(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dir := filepath.Join(parent, "staging")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tama-app.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateFile(dir, "tama-app.json", 64); err == nil {
		t.Fatal("accepted a permissive file")
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "tama-app.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateFile(dir, "tama-app.json", 64); err == nil {
		t.Fatal("accepted a permissive directory")
	}
}
