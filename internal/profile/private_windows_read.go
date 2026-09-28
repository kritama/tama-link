//go:build windows

package profile

import (
	"errors"
	"fmt"
	"io"

	"golang.org/x/sys/windows"
)

// ReadPrivateFile reads one regular file in dir without following links.
func ReadPrivateFile(dir, name string, limit int64) ([]byte, error) {
	if err := validateStagingName(name); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("read limit must be positive")
	}
	parent, err := openWindowsPrivateDir(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	handle, err := ntCreateAt(
		parent,
		name,
		windows.GENERIC_READ,
		windows.FILE_OPEN,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
	)
	if err != nil {
		return nil, err
	}
	file, err := checkedPrivateFile(handle, name, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return data, nil
}

func listProfileNames(dir string) ([]string, error) {
	parent, err := openWindowsPrivateDir(dir)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, nil
		}
		return nil, fmt.Errorf("open profile directory: %w", err)
	}
	defer func() { _ = parent.Close() }()
	infos, err := parent.Readdir(-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read profile directory: %w", err)
	}
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name())
	}
	return names, nil
}
