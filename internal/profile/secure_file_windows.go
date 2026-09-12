//go:build windows

package profile

import (
	"fmt"
	"os"
	"unsafe"

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
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
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
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("%s does not have a private DACL", path)
	}
	return validateProfileDACL(path, dacl, user.User.Sid)
}

const profileWriteMask windows.ACCESS_MASK = windows.GENERIC_WRITE | windows.GENERIC_ALL |
	windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
	windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES |
	windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
	0x40 // FILE_DELETE_CHILD for directories.

func validateProfileDACL(path string, dacl *windows.ACL, user *windows.SID) error {
	trusted, err := trustedProfileWriters(user)
	if err != nil {
		return fmt.Errorf("identify trusted Windows principals: %w", err)
	}
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return fmt.Errorf("inspect DACL entry %d for %s: %w", index, path, err)
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Mask&profileWriteMask == 0 {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			if isWindowsAllowACE(ace.Header.AceType) {
				return fmt.Errorf("%s grants write access through unsupported DACL entry type %d", path, ace.Header.AceType)
			}
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sidIn(sid, trusted) {
			return fmt.Errorf("%s grants write access to untrusted principal %s", path, sid.String())
		}
	}
	return nil
}

func trustedProfileWriters(user *windows.SID) ([]*windows.SID, error) {
	trusted := []*windows.SID{user}
	for _, kind := range []windows.WELL_KNOWN_SID_TYPE{
		windows.WinLocalSystemSid,
		windows.WinBuiltinAdministratorsSid,
		windows.WinCreatorOwnerSid,
		windows.WinCreatorOwnerRightsSid,
	} {
		sid, err := windows.CreateWellKnownSid(kind)
		if err != nil {
			return nil, err
		}
		trusted = append(trusted, sid)
	}
	return trusted, nil
}

func sidIn(candidate *windows.SID, trusted []*windows.SID) bool {
	for _, sid := range trusted {
		if candidate.Equals(sid) {
			return true
		}
	}
	return false
}

func isWindowsAllowACE(aceType uint8) bool {
	return aceType == 0x5 || aceType == 0x9 || aceType == 0xb
}
