//go:build windows

package store

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestOpenWindowsHandleAtUsesPinnedParent(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "original")
	replacement := filepath.Join(root, "replacement")
	for _, path := range []string{original, replacement} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("create directory %s: %v", path, err)
		}
	}

	parent, err := openWindowsTestDirectoryAllowDelete(original)
	if err != nil {
		t.Fatalf("open original directory: %v", err)
	}
	defer func() { _ = parent.Close() }()

	held := filepath.Join(root, "held")
	if err := os.Rename(original, held); err != nil {
		t.Fatalf("rename original directory: %v", err)
	}
	if err := os.Rename(replacement, original); err != nil {
		t.Fatalf("replace original pathname: %v", err)
	}

	file, err := openWindowsHandleAt(
		parent,
		"state.db",
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE,
		windows.FILE_OPEN_IF,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
	)
	if err != nil {
		t.Fatalf("open child relative to pinned parent: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close child: %v", err)
	}

	if _, err := os.Stat(filepath.Join(held, "state.db")); err != nil {
		t.Fatalf("child was not created under pinned parent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(original, "state.db")); !os.IsNotExist(err) {
		t.Fatalf("replacement pathname received child: %v", err)
	}
}

func openWindowsTestDirectoryAllowDelete(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}
