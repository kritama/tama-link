//go:build linux || darwin

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func sqlitePinnedPath(path *statePath) string {
	// /dev/fd resolves through the descriptor already opened with openat and
	// O_NOFOLLOW, so SQLite cannot select a replacement at path.name.
	return "/dev/fd/" + strconv.FormatUint(uint64(path.file.Fd()), 10)
}

func openStateHandles(parentPath, base string) (stateHandles, error) {
	directories, err := openStateDirectories(parentPath)
	if err != nil {
		return stateHandles{}, err
	}
	parent := directories[len(directories)-1]
	if err := secureStateParent(parentPath, parent); err != nil {
		closeStateDirectories(directories)
		return stateHandles{}, err
	}

	fileFD, err := openStateAt(int(parent.Fd()), base)
	if err != nil {
		closeStateDirectories(directories)
		return stateHandles{}, err
	}
	return stateHandles{
		ancestors: directories[:len(directories)-1],
		parent:    parent,
		file:      os.NewFile(uintptr(fileFD), base),
	}, nil
}

func openStateDirectories(path string) ([]*os.File, error) {
	rootFD, err := unix.Open(string(os.PathSeparator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	directories := []*os.File{os.NewFile(uintptr(rootFD), string(os.PathSeparator))}
	relative := strings.TrimPrefix(filepath.Clean(path), string(os.PathSeparator))
	if relative == "" {
		return directories, nil
	}
	for _, component := range strings.Split(relative, string(os.PathSeparator)) {
		fd, err := openStateDirectoryAt(int(directories[len(directories)-1].Fd()), component)
		if err != nil {
			closeStateDirectories(directories)
			return nil, err
		}
		directories = append(directories, os.NewFile(uintptr(fd), component))
	}
	return directories, nil
}

func openStateDirectoryAt(parentFD int, name string) (int, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Openat(parentFD, name, flags, 0)
	if err == nil {
		return fd, nil
	}
	if !errors.Is(err, unix.ENOENT) {
		return -1, err
	}
	if err := unix.Mkdirat(parentFD, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	return unix.Openat(parentFD, name, flags, 0)
}

func secureStateParent(parentPath string, parent *os.File) error {
	info, err := parent.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("inspect owner of state database parent %s", parentPath)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("state database parent %s is not owned by the current user", parentPath)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := parent.Chmod(0o700); err != nil {
			return err
		}
	}
	return nil
}

func openStateAt(parentFD int, base string) (int, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Openat(parentFD, base, flags, 0)
	if err == nil {
		return fd, nil
	}
	if !os.IsNotExist(err) {
		return -1, err
	}
	fd, err = unix.Openat(parentFD, base, flags|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err == nil {
		return fd, nil
	}
	if !os.IsExist(err) {
		return -1, err
	}
	return unix.Openat(parentFD, base, flags, 0)
}

func openSQLiteSidecar(path *statePath, base string) (*os.File, error) {
	fd, err := openStateAt(int(path.parent.Fd()), base)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), base), nil
}

func validatePrivateStateDir(path string, _ *os.File, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("state database parent %s permissions %04o are not private", path, info.Mode().Perm())
	}
	return nil
}

func validatePrivateStateFile(_ string, _ *os.File, _ os.FileInfo) error {
	return nil
}
