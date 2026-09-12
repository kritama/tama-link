//go:build windows

package profile

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func openProfileHandles(parentPath, base string) (*os.File, *os.File, error) {
	parent, err := openProfileHandle(
		parentPath,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
	)
	if err != nil {
		return nil, nil, err
	}
	file, err := openProfileHandle(
		parentPath+string(os.PathSeparator)+base,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
	)
	if err != nil {
		_ = parent.Close()
		return nil, nil, err
	}
	return parent, file, nil
}

func openProfileHandle(path string, attributes uint32) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		attributes,
		0,
	)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return nil, fmt.Errorf("profile path is a reparse point")
	}
	return os.NewFile(uintptr(handle), path), nil
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

	descriptor, err := windows.GetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("inspect owner of %s: %w", path, err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("read owner of %s: %w", path, err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current Windows user: %w", err)
	}
	if !owner.Equals(user.User.Sid) {
		return fmt.Errorf("%s is not owned by the current user", path)
	}
	return nil
}
