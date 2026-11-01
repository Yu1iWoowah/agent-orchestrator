// Package operatornative owns the private C0 issuer protocol. Browser commands,
// HTTP callers and session grants cannot enter this protocol's authority path.
package operatornative

import (
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
)

var errUnqualified = errors.New("operator native peer unqualified")

// A packaged app is the trust anchor for attach. A shared, unfused development
// Electron executable is deliberately insufficient. The packaged fuses prohibit
// RunAsNode, inspector entry and loading an alternative application. Same-uid
// replacement of installed code and OS process injection remain outside C0.
func packagedDesktop(daemon string) (string, error) {
	resources := filepath.Dir(filepath.Dir(daemon))
	if filepath.Base(resources) != "resources" && filepath.Base(resources) != "Resources" {
		return "", errUnqualified
	}
	if info, err := os.Stat(filepath.Join(resources, "app.asar")); err != nil || !info.Mode().IsRegular() {
		return "", errUnqualified
	}
	base := filepath.Dir(resources)
	if runtime.GOOS == "darwin" {
		base = filepath.Join(base, "MacOS")
	}
	name := "agent-orchestrator"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(base, name), nil
}

func fileDigest(path string) ([32]byte, error) {
	var result [32]byte
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return result, err
	}
	copy(result[:], hash.Sum(nil))
	return result, nil
}

func desktopArchive(executable string) string {
	base := filepath.Dir(executable)
	if runtime.GOOS == "darwin" {
		return filepath.Join(filepath.Dir(base), "Resources", "app.asar")
	}
	return filepath.Join(base, "resources", "app.asar")
}

// A desktop anchor pins the complete trusted app at daemon boot. Including the
// framework matters on macOS, where the launcher itself does not hold fuses.
type desktopAnchor struct{ executable, image, archive [32]byte }

func pinDesktop(daemon string) (desktopAnchor, error) {
	var anchor desktopAnchor
	expected, err := packagedDesktop(daemon)
	if err != nil || !qualifiedFuses(expected) {
		return anchor, errUnqualified
	}
	anchor.executable, err = fileDigest(expected)
	if err != nil {
		return anchor, errUnqualified
	}
	anchor.image, err = fileDigest(executableImage(expected))
	if err != nil {
		return anchor, errUnqualified
	}
	anchor.archive, err = fileDigest(desktopArchive(expected))
	return anchor, err
}

func (anchor desktopAnchor) verifyFiles(actual string) error {
	for _, item := range []struct {
		path   string
		digest [32]byte
	}{
		{actual, anchor.executable}, {executableImage(actual), anchor.image}, {desktopArchive(actual), anchor.archive},
	} {
		hash, err := fileDigest(item.path)
		if err != nil || hash != item.digest {
			return errUnqualified
		}
	}
	return nil
}

// Pin before the original AppImage mount disappears. Digests are non-secret,
// memory-only facts; OS peer identity, not a claimed path or PID, selects files.
func newNativePeerVerifier() func(net.Conn) error {
	self, err := os.Executable()
	if err != nil {
		return func(net.Conn) error { return errUnqualified }
	}
	anchor, err := pinDesktop(self)
	if err != nil {
		return func(net.Conn) error { return errUnqualified }
	}
	return func(conn net.Conn) error {
		actual, err := nativePeerExecutable(conn)
		if err != nil {
			return errUnqualified
		}
		return anchor.verifyFiles(actual)
	}
}
