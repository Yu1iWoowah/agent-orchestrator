//go:build windows

package cli

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func protectAppTestCredential(t *testing.T, name string) {
	t.Helper()
	sid, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(filepath.Dir(name)), filepath.Dir(name), name} {
		sd, err := windows.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;OICI;FA;;;" + sid.String() + ")(A;OICI;FA;;;SY)")
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, sid, nil, acl, nil); err != nil {
			t.Fatal(err)
		}
	}
}
func TestWindowsAppCredentialRejectsEveryoneACL(t *testing.T) {
	cfg := setConfigEnv(t)
	writeAppCredential(t, cfg.dataDir, testAppGrant)
	name := filepath.Join(cfg.dataDir, "diagnostics", "grants", testAppGrant+".json")
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(name, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAppCredential("next.js-1", testAppGrant); err == nil {
		t.Fatal("Everyone credential accepted")
	}
}
