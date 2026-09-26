//go:build linux || darwin

package bootstrap

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// withPublicationLock holds the cross-process publication lock across fn.
// discard and profile publication both take this lock, so one cannot remove
// the journal or credentials while the other is creating the profile file.
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
