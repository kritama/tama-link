//go:build linux || darwin

package profile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openDirNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openDirAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func ensurePrivateChild(parentPath, name string) error {
	parent, err := openDirNoFollow(parentPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", parentPath, err)
	}
	defer func() { _ = parent.Close() }()
	if err := validateOwnedDirectory(parentPath, parent, false); err != nil {
		return err
	}
	if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("create %s: %w", name, err)
	}
	child, err := openDirAt(parent, name)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer func() { _ = child.Close() }()
	return validateOwnedDirectory(parentPath+"/"+name, child, true)
}

func validateOwnedDirectory(path string, file *os.File, strict bool) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect owner of %s", path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is not owned by the current user", path)
	}
	perm := info.Mode().Perm()
	if strict && perm&0o077 != 0 {
		return fmt.Errorf("%s permissions %04o are not private", path, perm)
	}
	if !strict && perm&0o022 != 0 {
		return fmt.Errorf("%s is writable by other users", path)
	}
	return nil
}

func validatePrivateFile(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect owner of %s", path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is not owned by the current user", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s permissions %04o are not private", path, info.Mode().Perm())
	}
	return nil
}

// WritePrivateFile writes data to a new regular file in dir. replace false
// publishes with an atomic no-replace link so an existing target always
// wins. replace true is only for non-profile staging files.
func WritePrivateFile(dir, name string, data []byte, replace bool) error {
	if err := validateStagingName(name); err != nil {
		return err
	}
	parent, err := openDirNoFollow(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = parent.Close() }()
	if err := validateOwnedDirectory(dir, parent, true); err != nil {
		return err
	}
	tmp, err := randomStagingName(name)
	if err != nil {
		return err
	}
	fd, err := unix.Openat(int(parent.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create staging file: %w", err)
	}
	file := os.NewFile(uintptr(fd), tmp)
	defer func() { _ = file.Close() }()
	defer func() { _ = unix.Unlinkat(int(parent.Fd()), tmp, 0) }()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write staging file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("flush staging file: %w", err)
	}
	if replace {
		if err := unix.Renameat(int(parent.Fd()), tmp, int(parent.Fd()), name); err != nil {
			return fmt.Errorf("publish staging file: %w", err)
		}
	} else if err := unix.Linkat(int(parent.Fd()), tmp, int(parent.Fd()), name, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%w: %s", ErrExists, name)
		}
		return fmt.Errorf("publish %s: %w", name, err)
	}
	_ = parent.Sync()
	return nil
}

// RemovePrivateFile unlinks a regular file in dir without following links.
// A missing file is not an error. Directories and special files are left
// in place and reported.
func RemovePrivateFile(dir, name string) error {
	if err := validateStagingName(name); err != nil {
		return err
	}
	parent, err := openDirNoFollow(dir)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = parent.Close() }()
	if err := validateOwnedDirectory(dir, parent, true); err != nil {
		return err
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("open %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return fmt.Errorf("inspect %s: %w", name, statErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if err := validatePrivateFile(name, info); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// ReadPrivateFile reads one regular file in dir without following links.
func ReadPrivateFile(dir, name string, limit int64) ([]byte, error) {
	if err := validateStagingName(name); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("read limit must be positive")
	}
	parent, err := openDirNoFollow(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	if err := validateOwnedDirectory(dir, parent, true); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err := validatePrivateFile(name, info); err != nil {
		return nil, err
	}
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
	parent, err := openDirNoFollow(dir)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, nil
		}
		return nil, fmt.Errorf("open profile directory: %w", err)
	}
	defer func() { _ = parent.Close() }()
	if err := validateOwnedDirectory(dir, parent, true); err != nil {
		return nil, err
	}
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
