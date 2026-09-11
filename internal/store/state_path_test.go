package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatePathDetectsReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	secured, err := secureStatePath(path)
	if err != nil {
		t.Fatalf("secureStatePath: %v", err)
	}
	t.Cleanup(func() { _ = secured.Close() })

	if err := os.Rename(path, path+".replaced"); err != nil {
		t.Fatalf("rename pinned database: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create replacement database: %v", err)
	}
	if err := secured.Verify(); err == nil || !strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("Verify replacement = %v, want identity error", err)
	}
}

func TestStatePathRejectsSymlinkedParent(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}

	realParent := t.TempDir()
	parentLink := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(realParent, parentLink); err != nil {
		t.Fatalf("create parent symlink: %v", err)
	}
	if _, err := secureStatePath(filepath.Join(parentLink, "state.db")); err == nil {
		t.Fatal("secureStatePath accepted a symlinked parent")
	}
}
