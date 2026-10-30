package sessionmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/gitworktree"
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
	alternatePath := ""
	for _, action := range []string{"spawn", "restore", "relaunch"} {
		t.Run(action, func(t *testing.T) {
			rec := domain.SessionRecord{ID: "historical", ProjectID: "mer", Kind: domain.KindOrchestrator, Metadata: domain.SessionMetadata{WorkspacePath: old.Path, Branch: cfg.Branch}}
			var err error
			switch action {
			case "spawn":
				var next ports.WorkspaceInfo
				next, _, err = m.createSessionWorkspace(t.Context(), project, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindOrchestrator}, "replacement", cfg.Branch, nil)
				if err != nil || next.Path == old.Path || next.Path == "" || next.Branch == cfg.Branch {
					t.Fatalf("new orchestrator must use a distinct managed path and branch: old=%+v next=%+v err=%v", old, next, err)
				}
				alternatePath = next.Path
			case "restore":
				_, err = m.restoreSessionWorkspace(t.Context(), project, rec)
			case "relaunch":
				_, err = m.relaunchRestoredSession(t.Context(), rec, project, old)
			}
			var blocked *apierr.Error
			if action != "spawn" && (!errors.As(err, &blocked) || blocked.Code != "AGENT_SETTINGS_WRITER_UNCONFIRMED") {
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
	second, _, err := m.createSessionWorkspace(t.Context(), project, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindOrchestrator}, "replacement-2", cfg.Branch, nil)
	if err != nil || second.Path == old.Path || second.Path == alternatePath || second.Path == "" || second.Branch == cfg.Branch {
		t.Fatalf("later orchestrator must not collide with either retained or new session: %+v err=%v", second, err)
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

func TestOrchestratorFreshPathPreservesMultiRepoAndRejectsCollision(t *testing.T) {
	m, st, ws, repo := newGitTaskPreparationManager(t)
	project := st.projects["mer"]
	project.Kind = domain.ProjectKindWorkspace
	st.projects["mer"] = project
	child := filepath.Join(repo, "api")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	runManagerGit(t, child, "init")
	runManagerGit(t, child, "config", "user.name", "AO Test")
	runManagerGit(t, child, "config", "user.email", "ao@example.com")
	if err := os.WriteFile(filepath.Join(child, "README.md"), []byte("child"), 0o600); err != nil {
		t.Fatal(err)
	}
	runManagerGit(t, child, "add", ".")
	runManagerGit(t, child, "commit", "-m", "initial")
	runManagerGit(t, child, "branch", "-M", "main")
	st.workspaceRepo["mer"] = []domain.WorkspaceRepoRecord{{Name: "api", RelativePath: "api", GitStatus: domain.GitStatusReady}}
	old, err := ws.CreateWorkspaceProject(t.Context(), ports.WorkspaceProjectConfig{ProjectID: "mer", SessionID: "unknown", Kind: domain.KindOrchestrator, SessionPrefix: sessionPrefix(project), Branch: "ao/old", RootRepoPath: repo, BaseBranch: "main", Repos: []ports.WorkspaceProjectRepoConfig{{Name: "api", RelativePath: "api", RepoPath: child, BaseBranch: "main"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range old.Worktrees {
		if err := os.WriteFile(filepath.Join(row.Path, "retained.txt"), []byte(row.RepoName), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st.sessions["unknown"] = domain.SessionRecord{ID: "unknown", ProjectID: "mer", Kind: domain.KindOrchestrator, IsTerminated: true, Metadata: domain.SessionMetadata{WorkspacePath: old.Root.Path}}
	m.store = abandonedWorkspaceStore{st}
	orphan, err := ws.ResolveWorkspacePath(t.Context(), ports.WorkspaceConfig{ProjectID: "mer", SessionID: "replacement", Kind: domain.KindOrchestrator, SessionPrefix: sessionPrefix(project), FreshPath: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "orphan.txt"), []byte("unregistered data"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseRefs := map[string]string{repo: "refs/heads/main", child: "refs/heads/main"}
	next, nextProject, err := m.createSessionWorkspace(t.Context(), project, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindOrchestrator}, "replacement", old.Root.Branch, baseRefs)
	if err != nil || next.Path == old.Root.Path || nextProject == nil || len(nextProject.Worktrees) != 2 || next.Branch == old.Root.Branch || !strings.HasSuffix(next.Path, "-2") {
		t.Fatalf("multi-repo orchestrator must allocate a fresh root/branch: next=%+v project=%+v err=%v", next, nextProject, err)
	}
	for _, row := range nextProject.Worktrees {
		if row.Branch != next.Branch || strings.HasPrefix(row.Path+string(filepath.Separator), old.Root.Path+string(filepath.Separator)) {
			t.Fatalf("new repository aliases retained worktree: %+v", row)
		}
	}
	marker := filepath.Join(next.Path, "new-session.txt")
	if err := os.WriteFile(marker, []byte("new owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	another, anotherProject, err := m.createSessionWorkspace(t.Context(), project, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindOrchestrator}, "replacement", old.Root.Branch, baseRefs)
	if err != nil || another.Path == next.Path || another.Path == old.Root.Path || anotherProject == nil || len(anotherProject.Worktrees) != 2 || !strings.HasSuffix(another.Path, "-3") {
		t.Fatalf("occupied alternate must select the next validated path without reattaching: %+v err=%v", another, err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "new owner" {
		t.Fatalf("collision changed new owner's data: %q %v", data, err)
	}
	orphanData, orphanErr := os.ReadFile(filepath.Join(orphan, "orphan.txt"))
	if orphanErr != nil || string(orphanData) != "unregistered data" {
		t.Fatalf("unregistered occupied candidate changed: %q %v", orphanData, orphanErr)
	}
	for _, row := range old.Worktrees {
		data, err := os.ReadFile(filepath.Join(row.Path, "retained.txt"))
		if err != nil || string(data) != row.RepoName || strings.TrimSpace(runManagerGit(t, row.Path, "branch", "--show-current")) != row.Branch {
			t.Fatalf("retained repository changed: %+v data=%q err=%v", row, data, err)
		}
	}
}

type freshPathGitWorkspace struct {
	*gitworktree.Workspace
	created chan struct{}
}

func (w *freshPathGitWorkspace) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	close(w.created)
	return w.Workspace.Create(ctx, cfg)
}

func TestOrchestratorFreshPathPublicSpawnHoldsWorkspaceGate(t *testing.T) {
	m, st, ws, _ := newGitTaskPreparationManager(t)
	m.dataDir = t.TempDir()
	project := st.projects["mer"]
	old, err := ws.Create(t.Context(), ports.WorkspaceConfig{ProjectID: "mer", SessionID: "unknown", Kind: domain.KindOrchestrator, SessionPrefix: sessionPrefix(project), Branch: "ao/old", BaseBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	st.sessions["unknown"] = domain.SessionRecord{ID: "unknown", ProjectID: "mer", Kind: domain.KindOrchestrator, IsTerminated: true, Metadata: domain.SessionMetadata{WorkspacePath: old.Path}}
	m.store = abandonedWorkspaceStore{st}
	observed := &freshPathGitWorkspace{Workspace: ws, created: make(chan struct{})}
	m.workspace = observed
	release := sync.OnceFunc(m.acquireWorkspaceGate("mer"))
	defer release()
	type result struct {
		rec domain.SessionRecord
		err error
	}
	done := make(chan result, 1)
	go func() {
		rec, _, _, err := m.Spawn(t.Context(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindOrchestrator})
		done <- result{rec, err}
	}()
	select {
	case <-observed.created:
		t.Error("spawn reached workspace Create while another owner held its project gate")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	got := <-done
	if got.err != nil || got.rec.Metadata.WorkspacePath == "" || got.rec.Metadata.WorkspacePath == old.Path || !strings.Contains(got.rec.Metadata.WorkspacePath, string(got.rec.ID)) {
		t.Fatalf("public spawn did not persist a distinct session-owned path: %+v err=%v", got.rec, got.err)
	}
}
