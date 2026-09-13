//go:build windows

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	file, err := openWindowsHandle(
		parentPath+string(os.PathSeparator)+base,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
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
	currentPath := volume + string(os.PathSeparator)
	root, err := openWindowsDirectory(currentPath)
	if err != nil {
		return nil, err
	}
	directories := []*os.File{root}
	relative := strings.TrimLeft(clean[len(volume):], `/\`)
	for _, component := range strings.FieldsFunc(relative, isWindowsPathSeparator) {
		currentPath = filepath.Join(currentPath, component)
		if err := os.Mkdir(currentPath, 0o700); err != nil && !os.IsExist(err) {
			closeStateDirectories(directories)
			return nil, err
		}
		directory, err := openWindowsDirectory(currentPath)
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

func openSQLiteSidecar(path *statePath, base string) (*os.File, error) {
	return openWindowsHandle(
		filepath.Join(filepath.Dir(path.name), base),
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.OPEN_ALWAYS,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
	)
}

func validatePrivateStateDir(path string, file *os.File, _ os.FileInfo) error {
	return windowsacl.ValidatePrivate(path, windows.Handle(file.Fd()))
}

func validatePrivateStateFile(path string, file *os.File, _ os.FileInfo) error {
	return windowsacl.ValidatePrivate(path, windows.Handle(file.Fd()))
}
