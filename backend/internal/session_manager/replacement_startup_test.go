package sessionmanager

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementStartupFencesRetiredAndActiveSourceBeforeSideEffects(t *testing.T) {
	for _, phase := range []domain.ReplacementPhase{domain.ReplacementExportingTranscript, domain.ReplacementCompleted} {
		for _, action := range []string{"restore all", "reap"} {
			t.Run(string(phase)+"/"+action, func(t *testing.T) {
				m, base, runtime, workspace := newManager()
				source := mkLive("mer-old-source")
				source.Kind, source.IsTerminated = domain.KindOrchestrator, true
				source.Metadata.Branch, source.Metadata.WorkspacePath = "old-branch", t.TempDir()
				source.Metadata.RuntimeHandleID = "reused-runtime"
				base.sessions[source.ID] = source
				base.worktrees[source.ID] = []domain.SessionWorktreeRecord{{SessionID: source.ID, WorktreePath: source.Metadata.WorkspacePath, PreservedRef: "keep-preserved-ref"}}
				worker := mkLive("mer-worker")
				worker.Kind, worker.Harness = domain.KindWorker, domain.HarnessCodex
				worker.Metadata.RuntimeHandleID = "reused-runtime"
				base.sessions[worker.ID] = worker
				runtime.aliveByHandle = map[string]bool{"reused-runtime": true}
				m.store = &replacementFenceStore{fakeStore: base, retired: phase == domain.ReplacementCompleted, operation: domain.OrchestratorReplacement{ID: "replacement", ProjectID: source.ProjectID, SourceSessionID: source.ID, Phase: phase}}
				file := filepath.Join(source.Metadata.WorkspacePath, "untracked.txt")
				if err := os.WriteFile(file, []byte("preserve across restart"), 0o600); err != nil {
					t.Fatal(err)
				}
				var err error
				if action == "restore all" {
					err = m.RestoreAll(context.Background())
				} else {
					err = m.reconcileReap(context.Background(), source)
				}
				if err != nil {
					t.Fatal(err)
				}
				if runtime.created != 0 || runtime.destroyed != 0 || !runtime.aliveByHandle["reused-runtime"] || !reflect.DeepEqual(base.sessions[worker.ID], worker) {
					t.Fatal("startup touched source/worker runtime across replacement fence")
				}
				if len(workspace.restoreConfigs) != 0 || workspace.destroyed != 0 || len(base.worktrees[source.ID]) != 1 {
					t.Fatalf("startup touched retired workspace: restores=%d destroys=%d", len(workspace.restoreConfigs), workspace.destroyed)
				}
				bytes, err := os.ReadFile(file)
				if err != nil || string(bytes) != "preserve across restart" {
					t.Fatal("startup changed preserved source file", err)
				}
			})
		}
	}
}
