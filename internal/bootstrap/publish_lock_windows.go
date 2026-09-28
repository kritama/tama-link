//go:build windows

package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// withPublicationLock holds the cross-process bootstrap lock across fn.
// Session opening and lease claim, cleanup, discard, and publication all take
// this lock so cleanup cannot delete state another process is starting to use.
func withPublicationLock(configDir string, fn func() error) error {
	dir, err := prepareStaging(configDir)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, ".publication.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open publication lock: %w", err)
	}
	defer func() { _ = file.Close() }()
	if err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		1,
		0,
		&windows.Overlapped{},
	); err != nil {
		return fmt.Errorf("lock publication: %w", err)
	}
	defer func() {
		_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
	}()
	return fn()
}
