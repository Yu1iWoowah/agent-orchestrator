//go:build windows

package appdiagnostics

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"golang.org/x/sys/windows"
)

func TestWindowsPrivateDirectoryRejectsEveryoneAccess(t *testing.T) {
	name := filepath.Join(t.TempDir(), "private")
	if err := privateDir(name); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
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
	if err := privateDir(name); err == nil {
		t.Fatal("Everyone ACL accepted")
	}
	_ = os.Remove(name)
}

func TestWindowsPrivateStorePublishesAndReplacesOwnerOnlyFile(t *testing.T) {
	store, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.root.Close()
	id := uuid.NewString()
	for _, value := range []string{"first", "replacement"} {
		if err := store.write("grants", id, Credential{ID: id, Token: value}); err != nil {
			t.Fatalf("publish private file: %v", err)
		}
		var got Credential
		if err := store.read("grants", id, &got); err != nil {
			t.Fatalf("read private file: %v", err)
		}
		if got.Token != value {
			t.Fatal("replacement not published")
		}
	}
}

// Keep native CI evidence of legacy handle access and default ownership. This probe
// does not relax the assertions on the explicitly secured production path.
func TestWindowsLegacyFileCreationDiagnostic(t *testing.T) {
	store, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.root.Close()
	file, err := store.root.OpenFile(filepath.Join("grants", "legacy.tmp"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	t.Logf("legacy default-owner validation: %v", secureCreatedFile(file))
	sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Logf("legacy handle owner query: %v", err)
		return
	}
	owner, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("legacy default owner matches current user: %t", owner != nil && owner.Equals(user))
}
