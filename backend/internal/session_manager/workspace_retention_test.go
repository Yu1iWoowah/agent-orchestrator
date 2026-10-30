package sessionmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type abandonedWorkspaceStore struct{ *fakeStore }

var _ tuiSettingsTerminationStore = abandonedWorkspaceStore{}

func (abandonedWorkspaceStore) PendingAgentSettingsOperation(context.Context, domain.SessionID) (domain.AgentSettingsOperation, bool, error) {
	return domain.AgentSettingsOperation{}, false, errors.New("unexpected settings operation lookup")
}

func (abandonedWorkspaceStore) AbandonTerminatedTUISettings(context.Context, domain.SessionID, string, int64, bool, time.Time) (domain.AgentSettingsOperation, error) {
	return domain.AgentSettingsOperation{}, errors.New("unexpected settings operation settlement")
}

func (abandonedWorkspaceStore) HasAbandonedTUISettingsWriter(_ context.Context, id domain.SessionID) (bool, error) {
	return id == "unknown", nil
}

// The writer obligation becomes durable after admission but before rollback.
type lateAbandonedWorkspaceStore struct {
	abandonedWorkspaceStore
	published bool
}

func (s *lateAbandonedWorkspaceStore) UpsertSessionWorktree(ctx context.Context, row domain.SessionWorktreeRecord) error {
	s.published = true
	return s.fakeStore.UpsertSessionWorktree(ctx, row)
}

func (s *lateAbandonedWorkspaceStore) HasAbandonedTUISettingsWriter(_ context.Context, id domain.SessionID) (bool, error) {
	return s.published && id == "unknown", nil
}

func TestWorkspaceRowFailurePreservesSharedAbandonedWriter(t *testing.T) {
	m, st, _, ws := newManager()
	project := domain.ProjectRecord{ID: "mer", Kind: domain.ProjectKindWorkspace, Path: "/repo/mer"}
	st.projects["mer"] = project
	st.upsertWTErr = errors.New("db locked")
	ws.path = "/ws/shared"
	st.sessions["unknown"] = domain.SessionRecord{ID: "unknown", ProjectID: "mer", IsTerminated: true, Metadata: domain.SessionMetadata{WorkspacePath: ws.path}}
	m.store = &lateAbandonedWorkspaceStore{abandonedWorkspaceStore: abandonedWorkspaceStore{st}}
	_, retained, err := m.createSessionWorkspace(t.Context(), project, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker}, "attempt", "ao/attempt", nil)
	if err == nil || !strings.Contains(err.Error(), "record workspace worktree") {
		t.Fatalf("want row persistence error, got %v", err)
	}
	if ws.projectDestroyed != 0 || ws.destroyed != 0 || retained == nil {
		t.Fatalf("row rollback reclaimed abandoned writer path: projectDestroy=%d Destroy=%d retained=%v", ws.projectDestroyed, ws.destroyed, retained != nil)
	}
}

// ResolveWorkspacePath advertises the same canonical path that Create/Restore
// would reattach; admission must run before either materializes that path.
type retainedPathWorkspace struct{ *fakeWorkspace }

func (w retainedPathWorkspace) ResolveWorkspacePath(context.Context, ports.WorkspaceConfig) (string, error) {
	return w.path, nil
}

func TestWorkspaceAdmissionRejectsRetainedCanonicalPath(t *testing.T) {
	for _, kind := range []domain.ProjectKind{domain.ProjectKindSingleRepo, domain.ProjectKindWorkspace} {
		for _, action := range []string{"spawn", "restore"} {
			t.Run(string(kind)+"/"+action, func(t *testing.T) {
				m, st, rt, ws := newManager()
				project := domain.ProjectRecord{ID: "mer", Kind: kind, Path: "/repo/mer"}
				st.projects["mer"] = project
				ws.path = t.TempDir()
				st.sessions["unknown"] = domain.SessionRecord{ID: "unknown", ProjectID: "mer", Kind: domain.KindOrchestrator, IsTerminated: true, Metadata: domain.SessionMetadata{WorkspacePath: ws.path}}
				m.store = abandonedWorkspaceStore{st}
				m.workspace = retainedPathWorkspace{ws}
				var err error
				if action == "spawn" {
					_, _, err = m.createSessionWorkspace(t.Context(), project, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindOrchestrator}, "replacement", "ao/next", nil)
				} else {
					_, err = m.restoreSessionWorkspace(t.Context(), project, domain.SessionRecord{ID: "replacement", ProjectID: "mer", Kind: domain.KindOrchestrator, Metadata: domain.SessionMetadata{WorkspacePath: ws.path, Branch: "ao/next"}})
				}
				if err == nil || !strings.Contains(err.Error(), "unconfirmed") {
					t.Errorf("canonical path must report retained writer before reattach: %v", err)
				}
				if ws.createCount != 0 || ws.lastProjectCfg.SessionID != "" || len(ws.restoreConfigs) != 0 || rt.created != 0 {
					t.Fatalf("reattached retained canonical path: Create=%d project=%s Restore=%d runtime=%d", ws.createCount, ws.lastProjectCfg.SessionID, len(ws.restoreConfigs), rt.created)
				}
			})
		}
	}
}

func TestCanonicalGitWorkspaceRejectsReattachWhileWriterIsUnconfirmed(t *testing.T) {
	m, st, ws, _ := newGitTaskPreparationManager(t)
	project := st.projects["mer"]
	cfg := ports.WorkspaceConfig{ProjectID: "mer", SessionID: "unknown", Kind: domain.KindOrchestrator, SessionPrefix: sessionPrefix(project), Branch: "ao/old", BaseBranch: "main"}
	old, err := ws.Create(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(old.Path, "retained-work.txt")
	if err := os.WriteFile(marker, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	st.sessions["unknown"] = domain.SessionRecord{ID: "unknown", ProjectID: "mer", Kind: domain.KindOrchestrator, IsTerminated: true, Metadata: domain.SessionMetadata{WorkspacePath: old.Path}}
	m.store = abandonedWorkspaceStore{st}
	for _, action := range []string{"spawn", "restore", "relaunch"} {
		t.Run(action, func(t *testing.T) {
			rec := domain.SessionRecord{ID: "replacement", ProjectID: "mer", Kind: domain.KindOrchestrator, Metadata: domain.SessionMetadata{WorkspacePath: old.Path, Branch: cfg.Branch}}
			var err error
			switch action {
			case "spawn":
				_, _, err = m.createSessionWorkspace(t.Context(), project, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindOrchestrator}, rec.ID, "ao/next", nil)
			case "restore":
				_, err = m.restoreSessionWorkspace(t.Context(), project, rec)
			case "relaunch":
				_, err = m.relaunchRestoredSession(t.Context(), rec, project, old)
			}
			var blocked *apierr.Error
			if !errors.As(err, &blocked) || blocked.Code != "AGENT_SETTINGS_WRITER_UNCONFIRMED" {
				t.Fatalf("expected canonical writer veto before reattach: %v", err)
			}
			data, err := os.ReadFile(marker)
			if err != nil || string(data) != "retained" {
				t.Fatalf("retained work changed: %q %v", data, err)
			}
			if got := strings.TrimSpace(runManagerGit(t, old.Path, "branch", "--show-current")); got != cfg.Branch {
				t.Fatalf("reattach changed existing branch to %q", got)
			}
		})
	}
	worker, _, err := m.createSessionWorkspace(t.Context(), project, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker}, "worker-next", "ao/worker-next", nil)
	if err != nil || worker.Path == old.Path {
		t.Fatalf("an unrelated per-worker path should remain usable: %+v %v", worker, err)
	}
}

type workspaceRowsFailureStore struct{ *fakeStore }

var errWorkspaceRowsUnavailable = errors.New("rows unavailable")

func (workspaceRowsFailureStore) ListSessionWorktrees(context.Context, domain.SessionID) ([]domain.SessionWorktreeRecord, error) {
	return nil, errWorkspaceRowsUnavailable
}

func TestWorkspaceRowsFailureRetainsOperationContext(t *testing.T) {
	for _, operation := range []string{"retire replacement", "save"} {
		t.Run(operation, func(t *testing.T) {
			m, st, _, _ := newManager()
			rec := domain.SessionRecord{ID: "mer-1", ProjectID: "mer"}
			st.sessions[rec.ID] = rec
			m.store = workspaceRowsFailureStore{st}
			var err error
			if operation == "retire replacement" {
				err = m.RetireForReplacement(t.Context(), rec.ID)
			} else {
				err = m.saveAndTeardownOne(t.Context(), rec)
			}
			if !errors.Is(err, errWorkspaceRowsUnavailable) || !strings.Contains(err.Error(), operation+" mer-1: workspace rows:") {
				t.Fatalf("missing operation/session/error context: %v", err)
			}
		})
	}
}
