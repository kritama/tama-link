//go:build windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/kritama/tama-link/internal/windowsacl"
	"golang.org/x/sys/windows"
)

func sqlitePinnedPath(path *statePath) string {
	// The pinned file handle excludes FILE_SHARE_DELETE, so Windows cannot
	// replace this pathname while Store is open.
	return path.name
}

func openStateHandles(parentPath, base string) (stateHandles, error) {
	directories, err := openWindowsStateDirectories(parentPath)
	if err != nil {
		return stateHandles{}, err
	}
	parent := directories[len(directories)-1]
	file, err := openWindowsHandleAt(
		parent,
		base,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE,
		windows.FILE_OPEN_IF,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
	)
	if err != nil {
		closeStateDirectories(directories)
		return stateHandles{}, err
	}
	return stateHandles{
		ancestors: directories[:len(directories)-1],
		parent:    parent,
		file:      file,
	}, nil
}

func openWindowsStateDirectories(path string) ([]*os.File, error) {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	if volume == "" {
		return nil, fmt.Errorf("state database parent %s has no volume", path)
	}
	rootPath := volume + string(os.PathSeparator)
	root, err := openWindowsDirectory(rootPath)
	if err != nil {
		return nil, err
	}
	directories := []*os.File{root}
	relative := strings.TrimLeft(clean[len(volume):], `/\`)
	for _, component := range strings.FieldsFunc(relative, isWindowsPathSeparator) {
		directory, err := openWindowsHandleAt(
			directories[len(directories)-1],
			component,
			windows.FILE_GENERIC_READ,
			windows.FILE_OPEN_IF,
			windows.FILE_ATTRIBUTE_DIRECTORY,
			windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_OPEN_REPARSE_POINT,
		)
		if err != nil {
			closeStateDirectories(directories)
			return nil, err
		}
		directories = append(directories, directory)
	}
	return directories, nil
}

func openWindowsDirectory(path string) (*os.File, error) {
	return openWindowsHandle(
		path,
		windows.GENERIC_READ,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
	)
}

func isWindowsPathSeparator(value rune) bool {
	return value == '/' || value == '\\'
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
	return checkedWindowsFile(handle, path)
}

func openWindowsHandleAt(
	parent *os.File,
	name string,
	access, disposition, attributes, options uint32,
) (*os.File, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	objectAttributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(
		&handle,
		access,
		objectAttributes,
		&status,
		nil,
		attributes,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		disposition,
		options,
		0,
		0,
	); err != nil {
		return nil, err
	}
	return checkedWindowsFile(handle, name)
}

func checkedWindowsFile(handle windows.Handle, name string) (*os.File, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("path is a reparse point")
	}
	return os.NewFile(uintptr(handle), name), nil
}

func openSQLiteSidecar(path *statePath, base string) (*os.File, error) {
	return openWindowsHandleAt(
		path.parent,
		base,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE,
		windows.FILE_OPEN_IF,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
	)
}

func validatePrivateStateDir(path string, file *os.File, _ os.FileInfo) error {
	return windowsacl.ValidatePrivate(path, windows.Handle(file.Fd()))
}

func validatePrivateStateFile(path string, file *os.File, _ os.FileInfo) error {
	return windowsacl.ValidatePrivate(path, windows.Handle(file.Fd()))
}
