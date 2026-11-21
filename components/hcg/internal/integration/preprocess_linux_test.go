package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// Match audit I6's real file-open observation, using only this test's files.
func TestCancelledReportDoesNotOpenEngineFiles(t *testing.T) {
	executable, node := fixtureEngine(t, `process.exit(7)`)
	r := Runner{TempRoot: t.TempDir()}
	e, err := r.Register(t.Context(), executable, node, EngineVersion)
	if err != nil {
		t.Fatal(err)
	}
	id := testIdentity(t.TempDir())
	m, err := LaunchManifest(nil, id, PolicyInputs{Project: boolean(true)}, &e)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "native.jsonl"), []byte(claudeFixture), 0600); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, executable, unix.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out := r.Report(ctx, m, []AuthorizedSource{{Root: root, Path: "native.jsonl", Source: "claude", NativeID: id.NativeID}})
	buf := make([]byte, 4096)
	n, err := unix.Read(fd, buf)
	if n > 0 || err != nil && !errors.Is(err, unix.EAGAIN) || out.Reason != "cancelled" {
		t.Fatalf("cancelled report performed engine IO: eventBytes=%d err=%v reason=%s", n, err, out.Reason)
	}
}
