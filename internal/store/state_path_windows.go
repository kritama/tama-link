//go:build windows

package store

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func sqlitePinnedPath(path *statePath) string {
	// The pinned file handle excludes FILE_SHARE_DELETE, so Windows cannot
	// replace this pathname while Store is open.
	return path.name
}

func openStateHandles(parentPath, base string) (*os.File, *os.File, error) {
	parent, err := openWindowsHandle(
		parentPath,
		windows.GENERIC_READ,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
	)
	if err != nil {
		return nil, nil, err
	}
	file, err := openWindowsHandle(
		parentPath+string(os.PathSeparator)+base,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
	)
	if err != nil {
		_ = parent.Close()
		return nil, nil, err
	}
	return parent, file, nil
}

func openWindowsHandle(path string, access, creation, attributes uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		creation,
		attributes,
		0,
	)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("path is a reparse point")
	}
	return os.NewFile(uintptr(handle), path), nil
}

func validatePrivateStateDir(_ string, _ os.FileInfo) error {
	// Holding the directory handle without FILE_SHARE_DELETE prevents another
	// process from replacing it while this store is open. Windows access is
	// otherwise governed by the directory's ACL rather than Unix mode bits.
	return nil
}
