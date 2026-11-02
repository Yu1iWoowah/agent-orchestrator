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
type desktopAnchor struct {
	name    string
	digests [][32]byte
}

func desktopFiles(executable string) []string {
	files := []string{executable}
	if image := executableImage(executable); image != executable {
		files = append(files, image)
	}
	return append(files, desktopArchive(executable))
}

func pinDesktop(daemon string) (desktopAnchor, error) {
	var anchor desktopAnchor
	resolved, err := filepath.EvalSymlinks(daemon)
	if err != nil {
		return anchor, err
	}
	expected, err := packagedDesktop(resolved)
	if err != nil || !qualifiedFuses(expected) {
		return anchor, errUnqualified
	}
	anchor.name = filepath.Base(expected)
	for _, file := range desktopFiles(expected) {
		digest, err := fileDigest(file)
		if err != nil {
			return desktopAnchor{}, err
		}
		anchor.digests = append(anchor.digests, digest)
	}
	return anchor, nil
}

func (anchor desktopAnchor) verifyFiles(actual string) error {
	if filepath.Base(actual) != anchor.name {
		return errUnqualified
	}
	files := desktopFiles(actual)
	if len(files) != len(anchor.digests) {
		return errUnqualified
	}
	for i, file := range files {
		hash, err := fileDigest(file)
		if err != nil || hash != anchor.digests[i] {
			return errUnqualified
		}
	}
	return nil
}

// Capture the original bundle in the background before its AppImage mount
// disappears. Listener startup never waits on filesystem scans or hashing.
var errPinPending = errors.New("operator native bundle pin pending")

type nativePeerVerifier struct {
	done   chan struct{}
	anchor desktopAnchor
	err    error
}

func startNativePeerVerifier(pin func() (desktopAnchor, error)) *nativePeerVerifier {
	v := &nativePeerVerifier{done: make(chan struct{})}
	go func() {
		defer close(v.done)
		v.anchor, v.err = pin()
	}()
	return v
}

func newNativePeerVerifier() *nativePeerVerifier {
	return startNativePeerVerifier(func() (desktopAnchor, error) {
		self, err := os.Executable()
		if err != nil {
			return desktopAnchor{}, err
		}
		return pinDesktop(self)
	})
}

func (v *nativePeerVerifier) status() error {
	select {
	case <-v.done:
		if v.err != nil {
			return errUnqualified
		}
		return nil
	default:
		return errPinPending
	}
}

func (v *nativePeerVerifier) verify(conn net.Conn) error {
	if err := v.status(); err != nil {
		return err
	}
	actual, err := nativePeerExecutable(conn)
	if err != nil {
		return errUnqualified
	}
	return v.anchor.verifyFiles(actual)
}
