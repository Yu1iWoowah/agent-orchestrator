//go:build windows

package appdiagnostics

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func currentUserSID() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
}
func privateDir(name string) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;OICI;FA;;;" + sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		return err
	}
	ptr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	attrs := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(ptr, &attrs); err != nil && err != windows.ERROR_ALREADY_EXISTS {
		return err
	}
	handle, err := windows.CreateFile(ptr, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	return validateWindowsHandle(handle, true)
}
func privateDirectoryInfo(info os.FileInfo) error {
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return invalidFile("unsafe diagnostic directory")
	}
	return nil
}
func privateFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return invalidFile("unsafe diagnostic file")
	}
	return nil
}
func validateWindowsHandle(handle windows.Handle, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect diagnostic file identity: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (!directory && info.NumberOfLinks != 1) {
		return invalidFile("unsafe diagnostic file identity")
	}
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("inspect diagnostic security descriptor: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(sid) {
		return invalidFile("unsafe diagnostic owner")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount == 0 {
		return invalidFile("unsafe diagnostic ACL")
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return invalidFile("unsupported diagnostic ACL")
		}
		principal := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !principal.Equals(sid) && !principal.Equals(system) {
			return invalidFile("diagnostics ACL permits another account")
		}
	}
	return nil
}
func secureCreatedFile(file *os.File) error {
	return validateWindowsHandle(windows.Handle(file.Fd()), false)
}
func secureOpenedFile(file *os.File, directory bool) error {
	return validateWindowsHandle(windows.Handle(file.Fd()), directory)
}

// NtCreateFile is relative to the already-confined parent handle. Supplying an
// explicit owner avoids relying on the token's default owner (which may be Administrators)
// while retaining the current-user/SYSTEM-only protected DACL from creation.
func createPrivateFile(root *os.Root, name string) (*os.File, error) {
	parent, err := root.Open(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if err := secureOpenedFile(parent, true); err != nil {
		return nil, err
	}
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;;FA;;;" + sid.String() + ")(A;;FA;;;SY)")
	if err != nil {
		return nil, err
	}
	base, err := windows.NewNTUnicodeString(filepath.Base(name))
	if err != nil {
		return nil, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: windows.Handle(parent.Fd()), ObjectName: base, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE, SecurityDescriptor: sd}
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, windows.FILE_GENERIC_WRITE|windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, &attrs, &windows.IO_STATUS_BLOCK{}, nil, windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("create diagnostic private file: %w", err)
	}
	return os.NewFile(uintptr(handle), name), nil
}
