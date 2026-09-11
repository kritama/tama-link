package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// statePath pins handles to the profile directory and database inode. SQLite
// connections verify this identity before running any file-mutating PRAGMA.
type statePath struct {
	name   string
	parent *os.File
	file   *os.File
}

func secureStatePath(path string) (*statePath, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve state database %s: %w", path, err)
	}
	parent, file, err := openStateHandles(filepath.Dir(absolute), filepath.Base(absolute))
	if err != nil {
		return nil, classifyStatePathError(absolute, err)
	}
	secured := &statePath{name: absolute, parent: parent, file: file}
	if err := secured.validateHandles(); err != nil {
		_ = secured.Close()
		return nil, err
	}
	return secured, nil
}

func (p *statePath) validateHandles() error {
	parentInfo, err := p.parent.Stat()
	if err != nil {
		return fmt.Errorf("inspect state directory %s: %w", filepath.Dir(p.name), err)
	}
	if !parentInfo.IsDir() {
		return fmt.Errorf("state database parent %s is not a directory", filepath.Dir(p.name))
	}
	if err := validatePrivateStateDir(filepath.Dir(p.name), parentInfo); err != nil {
		return err
	}

	fileInfo, err := p.file.Stat()
	if err != nil {
		return fmt.Errorf("inspect state database %s: %w", p.name, err)
	}
	if !fileInfo.Mode().IsRegular() {
		return fmt.Errorf("state database %s is not a regular file", p.name)
	}
	if err := p.file.Chmod(0o600); err != nil {
		return fmt.Errorf("restrict state database %s: %w", p.name, err)
	}
	return nil
}

// Verify confirms that the current pathname still names the inode pinned by
// the no-follow handle. It is called immediately before and after every
// physical SQLite connection opens, before connection PRAGMAs may write.
func (p *statePath) Verify() error {
	linkInfo, err := os.Lstat(p.name)
	if err != nil {
		return fmt.Errorf("verify state database %s: %w", p.name, err)
	}
	if !linkInfo.Mode().IsRegular() {
		return fmt.Errorf("state database %s is not a regular file", p.name)
	}
	heldInfo, err := p.file.Stat()
	if err != nil {
		return fmt.Errorf("inspect open state database %s: %w", p.name, err)
	}
	if !os.SameFile(heldInfo, linkInfo) {
		return fmt.Errorf("state database %s changed while opening", p.name)
	}
	return nil
}

func (p *statePath) Close() error {
	if p == nil {
		return nil
	}
	return errors.Join(p.file.Close(), p.parent.Close())
}

func classifyStatePathError(path string, err error) error {
	info, statErr := os.Lstat(path)
	switch {
	case statErr == nil && info.IsDir():
		return fmt.Errorf("state database %s is a directory", path)
	case statErr == nil && !info.Mode().IsRegular():
		return fmt.Errorf("state database %s is not a regular file", path)
	case errors.Is(statErr, os.ErrNotExist):
		return fmt.Errorf("create state database %s: %w", path, err)
	default:
		return fmt.Errorf("open state database %s without following links: %w", path, err)
	}
}
