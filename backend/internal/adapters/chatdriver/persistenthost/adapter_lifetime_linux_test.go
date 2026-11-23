//go:build linux

package persistenthost

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestAdapterOwnerSurvivesDescriptorReplacementAndSettlesOnlyAfterExit(t *testing.T) {
	if os.Getenv("AO_TEST_ADAPTER_EXIT_CHILD") == "1" {
		var release [1]byte
		_, _ = os.Stdin.Read(release[:])
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAdapterOwnerSurvivesDescriptorReplacementAndSettlesOnlyAfterExit$")
	child.Env = append(os.Environ(), "AO_TEST_ADAPTER_EXIT_CHILD=1")
	release, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	configureProviderProcess(child)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release.Close(); _ = child.Wait() }()

	dataDir := t.TempDir()
	const session = "adapter-exit"
	dir, err := hostDir(dataDir, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// The original controller had a bound owner. On reconnect, a different
	// descriptor and launch can overwrite the current identity file.
	old := Descriptor{SessionID: session, PID: 31, StartedAt: time.Now().UTC()}
	if err := recordAdapterIdentity(dataDir, old, child.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := BindAdapterOwner(dataDir, session, deliveryEpoch(old), "conversation", "branch", "old"); err != nil {
		t.Fatal(err)
	}
	if err := writeAdapterRecord(dir, "adapter.json", adapterIdentity{Epoch: "new"}); err != nil {
		t.Fatal(err)
	}
	if err := BindAdapterOwner(dataDir, session, "new", "conversation", "branch", "new"); err != nil {
		t.Fatal(err)
	}
	var settled []string
	reconcile := func() {
		t.Helper()
		if err := ReconcileExitedAdapters(ctx, dataDir, func(id domain.SessionID, _ string) bool {
			return id == session
		}, func(_ context.Context, id domain.SessionID, conversation, branch, generation string, _ bool) (bool, error) {
			if id != session || conversation != "conversation" || branch != "branch" {
				t.Fatalf("unexpected adapter owner: %s/%s/%s", id, conversation, branch)
			}
			settled = append(settled, generation)
			return true, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	if len(settled) != 0 {
		t.Fatalf("settled while original adapter remained alive: %v", settled)
	}
	if err := release.Close(); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	reconcile()
	reconcile()
	if !slices.Equal(settled, []string{"old"}) {
		t.Fatalf("settled owners = %v, want old once", settled)
	}
}
