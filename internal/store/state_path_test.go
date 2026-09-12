package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSQLiteUsesPinnedStateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows prevents replacement by denying FILE_SHARE_DELETE")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	secured, err := secureStatePath(path)
	if err != nil {
		t.Fatalf("secureStatePath: %v", err)
	}
	t.Cleanup(func() { _ = secured.Close() })

	heldPath := path + ".held"
	if err := os.Rename(path, heldPath); err != nil {
		t.Fatalf("rename pinned database: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create replacement database: %v", err)
	}

	db, err := openSQLite(context.Background(), secured)
	if err != nil {
		t.Fatalf("openSQLite through pinned file: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE pinned_identity (value TEXT)"); err != nil {
		t.Fatalf("write pinned database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close pinned database: %v", err)
	}

	heldDB, err := sql.Open("sqlite", sqliteURI(heldPath))
	if err != nil {
		t.Fatalf("open held database: %v", err)
	}
	defer func() { _ = heldDB.Close() }()
	var table string
	if err := heldDB.QueryRow(
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'pinned_identity'",
	).Scan(&table); err != nil {
		t.Fatalf("pinned database marker: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat replacement database: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("replacement database size = %d, want untouched empty file", info.Size())
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
