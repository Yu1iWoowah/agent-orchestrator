package operatornative

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestNativePreflightUsesKernelExecutableBasename(t *testing.T) {
	address := filepath.Join(t.TempDir(), "s")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conn, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"agent-orchestrator", filepath.Base(exe)} {
		verifier := startNativePeerVerifier(func() (desktopAnchor, error) { return desktopAnchor{name: name}, nil })
		<-verifier.done
		err := verifier.preflight(conn)
		if (err == nil) != (name == filepath.Base(exe)) {
			t.Fatalf("kernel executable name %q: %v", name, err)
		}
		if verifier.verify(conn) == nil {
			t.Fatal("basename alone replaced code digest verification")
		}
	}
}

func TestDesktopVerificationRejectsFIFOWithoutBlocking(t *testing.T) {
	daemon, exe := installedFixture(t, t.TempDir())
	anchor, err := pinDesktop(daemon)
	if err != nil {
		t.Fatal(err)
	}
	archive := desktopArchive(exe)
	if err := os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(archive, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- anchor.verifyFiles(exe) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO archive accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO archive blocked verifier")
	}
	f, err := openDigestFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}
