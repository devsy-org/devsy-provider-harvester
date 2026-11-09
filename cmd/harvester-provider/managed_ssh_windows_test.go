package main

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestManagedWindowsACLRejectsAccessGrantedToAnotherAccount(t *testing.T) {
	root := filepath.Join(t.TempDir(), "credentials")
	identity, err := managedIdentityAt(root, "slot", true)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(identity.KeyPath, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := managedIdentityAt(root, "slot", false); err == nil {
		t.Fatal("private key with an Everyone read grant was accepted")
	}
}
