package main

import (
	"errors"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func secureManagedDirectory(path string) error {
	info, statErr := os.Lstat(path)
	if errors.Is(statErr, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return err
		}
		sid := user.User.Sid.String()
		sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")")
		if err != nil {
			return err
		}
		name, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return err
		}
		attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
		if err := windows.CreateDirectory(name, &attributes); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return err
		}
		info, statErr = os.Lstat(path)
	}
	if statErr != nil {
		return statErr
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed SSH directory must not be a symlink")
	}
	return checkManagedPermissions(path, info)
}

func checkManagedPermissions(path string, _ os.FileInfo) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		return errors.New("managed SSH state must belong to the current Windows user")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount != 1 {
		return errors.New("managed SSH state requires an owner-only Windows ACL")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, 0, &ace); err != nil {
		return err
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !sid.Equals(user.User.Sid) {
		return errors.New("managed SSH state grants access to another Windows account")
	}
	return nil
}

func tryManagedLock(file *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

func releaseManagedLock(file *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
}
