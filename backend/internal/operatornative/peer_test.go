package operatornative

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func installedFixture(t *testing.T, root string) (string, string) {
	t.Helper()
	resources := filepath.Join(root, "resources")
	base := root
	name := "agent-orchestrator"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if runtime.GOOS == "darwin" {
		resources = filepath.Join(root, "Contents", "Resources")
		base = filepath.Join(root, "Contents", "MacOS")
	}
	exe := filepath.Join(base, name)
	daemon := filepath.Join(resources, "daemon", "ao")
	for _, path := range []string{exe, daemon, desktopArchive(exe), executableImage(exe)} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("trusted fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	wire := append([]byte("dL7pKGdnNz796PbbjQWNKmHXBZaB9tsX"), 1, 8)
	wire = append(wire, []byte("00101100")...)
	if err := os.WriteFile(executableImage(exe), wire, 0600); err != nil {
		t.Fatal(err)
	}
	return daemon, exe
}

// This tests code pinning only, not OS peer identity or native UI qualification.
func TestDesktopPinSurvivesAppImageMountReplacement(t *testing.T) {
	oldRoot := filepath.Join(t.TempDir(), "old")
	daemon, _ := installedFixture(t, oldRoot)
	anchor, err := pinDesktop(daemon)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(oldRoot); err != nil {
		t.Fatal(err)
	}
	_, next := installedFixture(t, filepath.Join(t.TempDir(), "new"))
	if anchor.verifyFiles(next) != nil {
		t.Fatal("identical replacement mount rejected")
	}
	if err := os.WriteFile(desktopArchive(next), []byte("different application"), 0600); err != nil {
		t.Fatal(err)
	}
	if anchor.verifyFiles(next) == nil {
		t.Fatal("different app.asar accepted")
	}
	_, next = installedFixture(t, filepath.Join(t.TempDir(), "third"))
	if err := os.WriteFile(executableImage(next), []byte("different Electron runtime"), 0600); err != nil {
		t.Fatal(err)
	}
	if anchor.verifyFiles(next) == nil {
		t.Fatal("different executable/framework accepted")
	}
}
