//go:build linux || darwin

package store

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func openStateHandles(parentPath, base string) (*os.File, *os.File, error) {
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	parent := os.NewFile(uintptr(parentFD), parentPath)
	info, err := parent.Stat()
	if err != nil {
		_ = parent.Close()
		return nil, nil, err
	}
	if !info.IsDir() {
		_ = parent.Close()
		return nil, nil, fmt.Errorf("not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := parent.Chmod(0o700); err != nil {
			_ = parent.Close()
			return nil, nil, err
		}
	}

	fileFD, err := openStateAt(parentFD, base)
	if err != nil {
		_ = parent.Close()
		return nil, nil, err
	}
	return parent, os.NewFile(uintptr(fileFD), base), nil
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

func validatePrivateStateDir(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("state database parent %s permissions %04o are not private", path, info.Mode().Perm())
	}
	return nil
}
