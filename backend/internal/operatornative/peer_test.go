package operatornative

import (
	"errors"
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

func TestDesktopPinRejectsRenamedExecutable(t *testing.T) {
	daemon, exe := installedFixture(t, t.TempDir())
	anchor, err := pinDesktop(daemon)
	if err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(filepath.Dir(exe), "other-application")
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(renamed, data, 0600); err != nil {
		t.Fatal(err)
	}
	if anchor.verifyFiles(renamed) == nil {
		t.Fatal("renamed executable admitted")
	}
}

func TestDesktopPinRejectsSameSizeContentChange(t *testing.T) {
	daemon, exe := installedFixture(t, t.TempDir())
	anchor, err := pinDesktop(daemon)
	if err != nil {
		t.Fatal(err)
	}
	archive := desktopArchive(exe)
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	data[0] ^= 1
	if err := os.WriteFile(archive, data, 0600); err != nil {
		t.Fatal(err)
	}
	if anchor.verifyFiles(exe) == nil {
		t.Fatal("same-size replacement bypassed digest verification")
	}
}

func TestDesktopDigestsUseDistinctFiles(t *testing.T) {
	_, exe := installedFixture(t, t.TempDir())
	files := desktopFiles(exe)
	seen := map[string]bool{}
	for _, file := range files {
		if seen[file] {
			t.Fatal("same file hashed twice", file)
		}
		seen[file] = true
	}
	expected := 2
	if runtime.GOOS == "darwin" {
		expected = 3
	}
	if len(files) != expected {
		t.Fatalf("hash inputs: %v", files)
	}
}

func TestNativePinRunsInBackground(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	verifier := startNativePeerVerifier(func() (desktopAnchor, error) { close(started); <-release; return desktopAnchor{}, errUnqualified })
	<-started
	if !errors.Is(verifier.status(), errPinPending) {
		t.Fatal("background pin did not remain pending")
	}
	close(release)
	<-verifier.done
	if !errors.Is(verifier.status(), errUnqualified) {
		t.Fatal("failed pin did not remain closed")
	}
}

func TestDesktopPinResolvesDaemonSymlink(t *testing.T) {
	daemon, exe := installedFixture(t, t.TempDir())
	link := filepath.Join(t.TempDir(), "ao-link")
	if err := os.Symlink(daemon, link); err != nil {
		t.Skipf("symlink unavailable on this platform: %v", err)
	}
	anchor, err := pinDesktop(link)
	if err != nil || anchor.verifyFiles(exe) != nil {
		t.Fatalf("symlink launch did not resolve bundle: %v", err)
	}
}
