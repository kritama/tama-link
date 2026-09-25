//go:build windows

package profile

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"github.com/kritama/tama-link/internal/windowsacl"
	"golang.org/x/sys/windows"
)

type fileRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

func openWindowsPrivateDir(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	return checkedPrivateFile(handle, path, true)
}

func checkedPrivateFile(handle windows.Handle, path string, directory bool) (*os.File, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("%s is a reparse point", path)
	}
	isDir := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if directory != isDir {
		_ = windows.CloseHandle(handle)
		if directory {
			return nil, fmt.Errorf("%s is not a directory", path)
		}
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	file := os.NewFile(uintptr(handle), path)
	if err := windowsacl.ValidatePrivate(path, handle); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func ntCreateAt(parent *os.File, name string, access, disposition, attributes, options uint32) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	objectAttributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
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
	)
	return handle, err
}

func ensurePrivateChild(parentPath, name string) error {
	parent, err := openWindowsPrivateDir(parentPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", parentPath, err)
	}
	defer func() { _ = parent.Close() }()
	handle, err := ntCreateAt(
		parent,
		name,
		windows.GENERIC_READ,
		windows.FILE_OPEN_IF,
		windows.FILE_ATTRIBUTE_DIRECTORY,
		windows.FILE_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
	)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	child, err := checkedPrivateFile(handle, name, true)
	if err != nil {
		return err
	}
	return child.Close()
}

// WritePrivateFile writes data to a new regular file in dir. replace false
// uses an atomic no-replace rename so an existing target always wins.
func WritePrivateFile(dir, name string, data []byte, replace bool) error {
	if err := validateStagingName(name); err != nil {
		return err
	}
	parent, err := openWindowsPrivateDir(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = parent.Close() }()
	tmp, err := randomStagingName(name)
	if err != nil {
		return err
	}
	handle, err := ntCreateAt(
		parent,
		tmp,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
		windows.FILE_CREATE,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
	)
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}
	file, err := checkedPrivateFile(handle, tmp, false)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write staging file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("flush staging file: %w", err)
	}
	if err := renameWindowsNoFollow(file, parent, name, replace); err != nil {
		return err
	}
	_ = parent.Sync()
	return nil
}

func renameWindowsNoFollow(file, parent *os.File, name string, replace bool) error {
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	fileNameLen := len(encoded)*2 - 2
	var dummy fileRenameInformation
	bufferSize := int(unsafe.Offsetof(dummy.FileName)) + fileNameLen
	buffer := make([]byte, bufferSize)
	info := (*fileRenameInformation)(unsafe.Pointer(&buffer[0]))
	if replace {
		info.ReplaceIfExists = windows.FILE_RENAME_REPLACE_IF_EXISTS
	}
	info.RootDirectory = windows.Handle(parent.Fd())
	info.FileNameLength = uint32(fileNameLen)
	copy((*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&info.FileName[0]))[:fileNameLen/2:fileNameLen/2], encoded)
	var status windows.IO_STATUS_BLOCK
	err = windows.NtSetInformationFile(
		windows.Handle(file.Fd()),
		&status,
		&buffer[0],
		uint32(bufferSize),
		windows.FileRenameInformation,
	)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) {
		return fmt.Errorf("%w: %s", ErrExists, name)
	}
	return fmt.Errorf("publish %s: %w", name, err)
}

// RemovePrivateFile unlinks a regular file in dir without following links.
func RemovePrivateFile(dir, name string) error {
	if err := validateStagingName(name); err != nil {
		return err
	}
	parent, err := openWindowsPrivateDir(dir)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = parent.Close() }()
	handle, err := ntCreateAt(
		parent,
		name,
		windows.DELETE,
		windows.FILE_OPEN,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return fmt.Errorf("open %s: %w", name, err)
	}
	file, err := checkedPrivateFile(handle, name, false)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	deleteFile := byte(1)
	var status windows.IO_STATUS_BLOCK
	err = windows.NtSetInformationFile(
		windows.Handle(file.Fd()),
		&status,
		&deleteFile,
		1,
		windows.FileDispositionInformation,
	)
	if err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}
