//go:build windows

// Package windowsacl validates restrictive Windows file-system permissions.
package windowsacl

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ValidatePrivate requires current-user ownership and rejects write-capable
// DACL entries for non-privileged principals.
func ValidatePrivate(path string, handle windows.Handle) error {
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("inspect security of %s: %w", path, err)
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
	return validateDACL(path, dacl, user.User.Sid)
}

const writeMask windows.ACCESS_MASK = windows.GENERIC_WRITE | windows.GENERIC_ALL |
	windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
	windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES |
	windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
	0x40 // FILE_DELETE_CHILD for directories.

func validateDACL(path string, dacl *windows.ACL, user *windows.SID) error {
	trusted, err := trustedWriters(user)
	if err != nil {
		return fmt.Errorf("identify trusted Windows principals: %w", err)
	}
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return fmt.Errorf("inspect DACL entry %d for %s: %w", index, path, err)
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || ace.Mask&writeMask == 0 {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			if isAllowACE(ace.Header.AceType) {
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

func trustedWriters(user *windows.SID) ([]*windows.SID, error) {
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

func isAllowACE(aceType uint8) bool {
	return aceType == 0x5 || aceType == 0x9 || aceType == 0xb
}
