//go:build linux || darwin

package bootstrap

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// withPublicationLock holds the cross-process bootstrap lock across fn.
// Session opening and lease claim, cleanup, discard, and publication all take
// this lock so cleanup cannot delete state another process is starting to use.
func withPublicationLock(configDir string, fn func() error) error {
	dir, err := prepareStaging(configDir)
	if err != nil {
		return err
	}
	dirFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open publication directory: %w", err)
	}
	directory := os.NewFile(uintptr(dirFD), dir)
	defer func() { _ = directory.Close() }()
	fd, err := unix.Openat(int(directory.Fd()), ".publication.lock", unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("open publication lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), ".publication.lock")
	defer func() { _ = file.Close() }()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock publication: %w", err)
	}
	defer func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }()
	return fn()
}
