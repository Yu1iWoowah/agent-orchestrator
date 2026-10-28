//go:build windows

package appdiagnostics

import (
	"errors"
	"os"
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
		return errors.New("unsafe diagnostic directory")
	}
	return nil
}
func privateFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("unsafe diagnostic file")
	}
	return nil
}
func validateWindowsHandle(handle windows.Handle, directory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (!directory && info.NumberOfLinks != 1) {
		return errors.New("unsafe diagnostic file identity")
	}
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(sid) {
		return errors.New("unsafe diagnostic owner")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount == 0 {
		return errors.New("unsafe diagnostic ACL")
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
			return errors.New("unsupported diagnostic ACL")
		}
		principal := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !principal.Equals(sid) && !principal.Equals(system) {
			return errors.New("diagnostics ACL permits another account")
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
