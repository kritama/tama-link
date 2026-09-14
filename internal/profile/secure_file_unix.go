//go:build linux || darwin

package profile

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func openProfileHandles(parentPath, base string) (*os.File, *os.File, error) {
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	parent := os.NewFile(uintptr(parentFD), parentPath)

	fileFD, err := unix.Openat(parentFD, base, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		_ = parent.Close()
		return nil, nil, err
	}
	return parent, os.NewFile(uintptr(fileFD), base), nil
}

func validateProfileHandle(path string, file *os.File, directory bool) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if directory && !info.IsDir() {
		return fmt.Errorf("profile directory %s is not a directory", path)
	}
	if !directory && info.IsDir() {
		return fmt.Errorf("profile path %s is a directory", path)
	}
	if !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("profile path %s is not a regular file", path)
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
