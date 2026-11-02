//go:build linux

package operatornative

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestC0OwnerProcess(t *testing.T) {
	if os.Getenv("C0_NATIVE_OWNER_CHILD") != "1" {
		t.Skip("owner lifecycle fixture child only")
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func ownerProcessFixture(t *testing.T) (*Owner, func()) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestC0OwnerProcess$")
	cmd.Env = append(os.Environ(), "C0_NATIVE_OWNER_CHILD=1")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := sync.OnceFunc(func() { _ = input.Close(); _ = cmd.Wait() })
	t.Cleanup(stop)
	owner := watchOwnerPID(cmd.Process.Pid)
	t.Cleanup(func() { _ = owner.Close() })
	if owner.exited == nil {
		t.Fatal("pidfd unavailable: cannot prove owner exit")
	}
	if owner.ended() {
		t.Fatal("live owner reported exited")
	}
	return owner, stop
}

func TestOwnedTokenRequiredUntilOriginalOwnerExits(t *testing.T) {
	owner, stop := ownerProcessFixture(t)
	b, s, _ := nativeFixture(t)
	b.owner = owner
	rejectMissing := func() {
		c := dialNative(t, b, frame{Type: "operator-hello", Version: 1})
		if out := c.receive(t); out.Type != "operator-hello-rejected" || out.Code != "OPERATOR_TOKEN_REQUIRED" {
			t.Fatalf("live owner lost token requirement: %+v", out)
		}
		<-c.done
		b.mu.Lock()
		occupied := b.conn != nil
		b.mu.Unlock()
		if occupied {
			t.Fatal("tokenless second instance acquired issuer slot")
		}
	}
	rejectMissing()
	c := readyNative(t, b)
	_ = c.conn.Close()
	<-c.done
	// The owner process lives across this reconnect gap. A second instance
	// cannot take the slot just because the native connection has closed.
	rejectMissing()
	c = readyNative(t, b)
	_ = c.conn.Close()
	<-c.done
	stop()
	if !owner.ended() {
		t.Fatal("pidfd did not report original owner exit")
	}
	attached := dialNative(t, b, frame{Type: "operator-hello", Version: 1})
	if out := attached.receive(t); out.Type != "operator-hello-ok" {
		t.Fatalf("attach after owner exit: %+v", out)
	}
	if out := attached.call(t, frame{Type: "ready"}); out.Type != "pong" {
		t.Fatal("attach not ready")
	}
	plan := planFor(domain.ConsentSettingsPermissions)
	if _, err := s.PrepareOperatorAction(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	review := attached.call(t, frame{Type: "review-next"})
	if out := attached.call(t, frame{Type: "confirm", RequestRef: review.RequestRef, Challenge: review.Challenge, Digest: review.Digest}); out.Type != "confirmed" {
		t.Fatalf("qualified attached protocol failed: %+v", out)
	}
}

func TestOwnerUnknownOrClosedDoesNotProveExit(t *testing.T) {
	if (*Owner)(nil).ended() || (&Owner{}).ended() || watchOwnerPID(-1).ended() {
		t.Fatal("unknown owner unlocked attach")
	}
	owner, stop := ownerProcessFixture(t)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	stop()
	if owner.ended() {
		t.Fatal("closed watch was treated as proof of death")
	}
}

func TestNativeSocketDeadlineUsesWallClock(t *testing.T) {
	b, _, _ := nativeFixture(t)
	b.now = func() time.Time { return time.Unix(1, 0) }
	_ = readyNative(t, b)
	plan := planFor(domain.ConsentSettingsPermissions)
	if !b.GetAuthorityReadiness([]domain.OperatorPurpose{plan.Purpose}, plan.Scope).AuthorityGo {
		t.Fatal("fake policy clock broke real socket deadline")
	}
}

func TestOwnerBundleIdentityRejectsSameNamedCopy(t *testing.T) {
	_, first := installedFixture(t, t.TempDir())
	_, copiedExe := installedFixture(t, t.TempDir())
	if !sameOwnerBundle(first, desktopArchive(first), first, desktopArchive(first)) {
		t.Fatal("original bundle not recognized")
	}
	if sameOwnerBundle(copiedExe, desktopArchive(copiedExe), first, desktopArchive(first)) {
		t.Fatal("same-named copied process treated as original owner")
	}
	if sameOwnerBundle(first, desktopArchive(copiedExe), first, desktopArchive(first)) {
		t.Fatal("alternative archive treated as original owner")
	}
}
