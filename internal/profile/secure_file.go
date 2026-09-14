package profile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func readProfileFile(path string) ([]byte, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve profile path: %w", err)
	}
	parent, file, err := openProfileHandles(filepath.Dir(absolute), filepath.Base(absolute))
	if err != nil {
		return nil, classifyProfileOpenError(absolute, err)
	}
	defer func() { _ = parent.Close() }()
	defer func() { _ = file.Close() }()

	if err := validateProfileHandle(filepath.Dir(absolute), parent, true); err != nil {
		return nil, err
	}
	if err := validateProfileHandle(absolute, file, false); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect profile file: %w", err)
	}
	if info.Size() > maxProfileFileBytes {
		return nil, fmt.Errorf("profile exceeds %d bytes", maxProfileFileBytes)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxProfileFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxProfileFileBytes {
		return nil, fmt.Errorf("profile exceeds %d bytes", maxProfileFileBytes)
	}
	return data, nil
}

func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}

func classifyProfileOpenError(path string, err error) error {
	info, statErr := os.Lstat(path)
	if statErr == nil {
		if info.IsDir() {
			return fmt.Errorf("profile path %s is a directory", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("profile path %s is not a regular file", path)
		}
	}
	return err
}
