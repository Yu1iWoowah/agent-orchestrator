package sessionmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementAccountObservingChat struct {
	*replacementPreflightChat
	selected domain.ResolvedAgentAccount
	found    bool
}

func (c *replacementAccountObservingChat) StartChat(ctx context.Context, cfg ChatStart) (ChatStarted, error) {
	c.selected, c.found = ports.AgentAccountFromContext(ctx)
	return c.replacementPreflightChat.StartChat(ctx, cfg)
}

type replacementBranchCheckingWorkspace struct {
	*fakeWorkspace
	branches []string
}

func (w *replacementBranchCheckingWorkspace) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	w.branches = append(w.branches, cfg.Branch)
	if cfg.Branch == "" {
		return ports.WorkspaceInfo{}, errors.New("branch is required")
	}
	return w.fakeWorkspace.Create(ctx, cfg)
}

func TestReplacementAsyncFailureRetryReusesSameUnlaunchedSeed(t *testing.T) {
	m, store, chat, deferred := replacementSuccessorFixture(t)
	ctx := context.Background()
	observed := &replacementAccountObservingChat{replacementPreflightChat: chat}
	m.chat = observed
	workspace := m.workspace.(*fakeWorkspace)
	checked := &replacementBranchCheckingWorkspace{fakeWorkspace: workspace}
	m.workspace = checked
	workspace.createErr = errors.New("workspace preparation unavailable")
	seed, err := m.EnsureReplacementSuccessor(ctx, store.operation)
	if err != nil {
		t.Fatal(err)
	}
	(*deferred)[0]()
	if store.sessions[seed.ID].ProvisionState != domain.SessionProvisionFailed || len(chat.started) != 0 {
		t.Fatal("fixture did not fail before provider launch")
	}
	if store.sessions[seed.ID].Metadata.Branch != "" || len(checked.branches) != 1 || checked.branches[0] == "" {
		t.Fatal("fixture must fail before the deterministic branch is published", checked.branches)
	}
	if _, err := m.EnsureReplacementSuccessor(ctx, store.operation); err == nil || len(*deferred) != 1 {
		t.Fatal("polling retried provisioning without explicit authorization")
	}
	if len(chat.stopped) != 0 {
		t.Fatal("prelaunch failure stopped a controller")
	}
	// The coordinator persists this authorization only on explicit Retry.
	store.operation.SpawnRetryGeneration++
	workspace.createErr = nil
	retry, err := m.EnsureReplacementSuccessor(ctx, store.operation)
	if err != nil || retry.ID != seed.ID || len(*deferred) != 2 || store.creates != 1 {
		t.Fatal("prelaunch retry did not reuse the bound successor", retry.ID, err)
	}
	(*deferred)[1]()
	if len(checked.branches) != 2 || checked.branches[1] != checked.branches[0] {
		t.Fatal("retry lost the deterministic replacement branch", checked.branches)
	}
	if !observed.found || observed.selected.AccountID != store.operation.TargetAccountID {
		t.Fatalf("retry lost pinned account context: found=%t selected=%s want=%s", observed.found, observed.selected.AccountID, store.operation.TargetAccountID)
	}
	if len(chat.started) != 1 || store.creates != 1 || store.sessions[seed.ID].ProvisionState != domain.SessionProvisionReady {
		t.Fatal("same seed retry did not start one controller")
	}
}

type lostReplacementLaunchReply struct{ *replacementSuccessorStore }

func TestReplacementRetryCommitsPreviouslyUncommittedClientRequest(t *testing.T) {
	m, store, chat, deferred := replacementSuccessorFixture(t)
	ctx := context.Background()
	workspace := m.workspace.(*fakeWorkspace)
	workspace.createErr = errors.New("workspace failed before launch")
	seed, err := m.EnsureReplacementSuccessor(ctx, store.operation)
	if err != nil {
		t.Fatal(err)
	}
	(*deferred)[0]()
	rec := store.sessions[seed.ID]
	if rec.ProvisionState != domain.SessionProvisionFailed {
		t.Fatal("fixture did not fail before launch")
	}
	// Model a crash before the initial client request commit.
	rec.ClientRequestCommitted = false
	store.sessions[seed.ID] = rec
	store.operation.SpawnRetryGeneration++
	workspace.createErr = nil
	retried, err := m.EnsureReplacementSuccessor(ctx, store.operation)
	if err != nil || retried.ID != seed.ID || !store.sessions[seed.ID].ClientRequestCommitted {
		t.Fatal("authorized retry returned without committing its client request", err)
	}
	(*deferred)[1]()
	if store.sessions[seed.ID].ProvisionState != domain.SessionProvisionReady || store.creates != 1 || len(chat.started) != 1 {
		t.Fatal("request commit replayed or lost the one controller")
	}
}

func (s lostReplacementLaunchReply) MarkReplacementSpawnLaunch(ctx context.Context, operation string, id domain.SessionID, attempt string, at time.Time) (bool, error) {
	if applied, err := s.replacementSuccessorStore.MarkReplacementSpawnLaunch(ctx, operation, id, attempt, at); err != nil || !applied {
		return applied, err
	}
	return false, errors.New("launch intent reply lost")
}

func TestReplacementLaunchIntentLostReplyNeverCallsProviderOrRetries(t *testing.T) {
	m, store, chat, deferred := replacementSuccessorFixture(t)
	m.store = lostReplacementLaunchReply{replacementSuccessorStore: store}
	seed, err := m.EnsureReplacementSuccessor(context.Background(), store.operation)
	if err != nil {
		t.Fatal(err)
	}
	(*deferred)[0]()
	attempt, exists := store.operation.LatestSpawnAttempt()
	if !exists || attempt.LaunchIntentAt == nil || len(chat.started) != 0 || len(chat.stopped) != 0 {
		t.Fatal("lost launch receipt reached provider or lost intent")
	}
	if store.sessions[seed.ID].ProvisionState != domain.SessionProvisionFailed {
		t.Fatal("uncertain launch is not visible")
	}
	// Even a caller claiming a newer authorization cannot erase launch intent.
	store.operation.SpawnRetryGeneration++
	if _, err := m.EnsureReplacementSuccessor(context.Background(), store.operation); err == nil || len(*deferred) != 1 || store.creates != 1 {
		t.Fatal("uncertain launch retried provider or created another seed", err)
	}
}

type replacementWorkspaceChange struct {
	*fakeWorkspace
	change func()
}

func (w replacementWorkspaceChange) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	ws, err := w.fakeWorkspace.Create(ctx, cfg)
	if err == nil {
		w.change()
	}
	return ws, err
}

func TestReplacementObsoleteBackgroundCannotPublishNewAttemptWorkspace(t *testing.T) {
	m, st, chat, deferred := replacementSuccessorFixture(t)
	m.workspace = replacementWorkspaceChange{fakeWorkspace: m.workspace.(*fakeWorkspace), change: func() {
		now := time.Now().UTC()
		old := len(st.operation.SpawnAttempts) - 1
		st.operation.SpawnAttempts[old].FailedAt = &now
		st.operation.SpawnRetryGeneration++
		st.operation.SpawnAttempts = append(st.operation.SpawnAttempts, domain.ReplacementSpawnAttempt{ID: "new-attempt", RetryGeneration: st.operation.SpawnRetryGeneration, StartedAt: now})
		st.operation.Revision++
		rec := st.sessions[st.operation.SuccessorSessionID]
		rec.Metadata.WorkspacePath = "/new-attempt/workspace"
		rec.ProvisionSteps = []domain.SessionProvisionStep{{ID: domain.SessionProvisionStepAgent, Status: domain.SessionProvisionStepRunning}}
		st.sessions[rec.ID] = rec
	}}
	seed, err := m.EnsureReplacementSuccessor(context.Background(), st.operation)
	if err != nil {
		t.Fatal(err)
	}
	(*deferred)[0]()
	rec := st.sessions[seed.ID]
	if rec.Metadata.WorkspacePath != "/new-attempt/workspace" || len(rec.ProvisionSteps) != 1 || rec.ProvisionSteps[0].ID != domain.SessionProvisionStepAgent || rec.ProvisionSteps[0].Status != domain.SessionProvisionStepRunning || rec.ProvisionState != domain.SessionProvisionProvisioning || len(chat.started) != 0 {
		t.Fatalf("obsolete background overwrote new attempt: path=%s steps=%+v state=%s starts=%d", rec.Metadata.WorkspacePath, rec.ProvisionSteps, rec.ProvisionState, len(chat.started))
	}
}
