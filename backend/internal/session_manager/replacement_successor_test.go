package sessionmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	browsersvc "github.com/aoagents/agent-orchestrator/backend/internal/service/browser"
)

type replacementSuccessorStore struct {
	*replacementFenceStore
	creates int
}

func (s *replacementSuccessorStore) BeginReplacementSpawnAttempt(_ context.Context, operation string, id domain.SessionID, attemptID string, at time.Time) (domain.OrchestratorReplacement, domain.SessionRecord, error) {
	rec, found := s.sessions[id]
	if !found || s.operation.ID != operation || s.operation.Phase != domain.ReplacementStarting || s.operation.SuccessorSessionID != id || rec.IsTerminated || rec.Metadata.ControllerGeneration != "" || rec.Metadata.ProviderConversationID != "" {
		return s.operation, rec, domain.ErrReplacementStateInvalid
	}
	s.operation.SpawnAttempts = append([]domain.ReplacementSpawnAttempt(nil), s.operation.SpawnAttempts...)
	if prior, exists := s.operation.LatestSpawnAttempt(); exists {
		if prior.LaunchIntentAt != nil || prior.RetryGeneration >= s.operation.SpawnRetryGeneration {
			return s.operation, rec, domain.ErrReplacementStateInvalid
		}
		if prior.FailedAt == nil {
			s.operation.SpawnAttempts[len(s.operation.SpawnAttempts)-1].FailedAt = &at
		}
	}
	s.operation.SpawnAttempts = append(s.operation.SpawnAttempts, domain.ReplacementSpawnAttempt{ID: attemptID, StartedAt: at, RetryGeneration: s.operation.SpawnRetryGeneration})
	s.operation.Revision++
	rec.ProvisionState, rec.ProvisionError = domain.SessionProvisionProvisioning, ""
	s.sessions[id] = rec
	return s.operation, rec, nil
}

func (s *replacementSuccessorStore) MarkReplacementSpawnLaunch(_ context.Context, operation string, id domain.SessionID, attemptID string, at time.Time) (bool, error) {
	attempt, exists := s.operation.LatestSpawnAttempt()
	if !exists || s.operation.ID != operation || s.operation.SuccessorSessionID != id || s.operation.Phase != domain.ReplacementStarting || attempt.ID != attemptID || attempt.LaunchIntentAt != nil || attempt.FailedAt != nil {
		return false, nil
	}
	s.operation.SpawnAttempts[len(s.operation.SpawnAttempts)-1].LaunchIntentAt = &at
	s.operation.Revision++
	return true, nil
}

func (s *replacementSuccessorStore) FailReplacementSpawnAttempt(_ context.Context, operation string, id domain.SessionID, attemptID, message string, at time.Time) (bool, error) {
	attempt, exists := s.operation.LatestSpawnAttempt()
	rec, found := s.sessions[id]
	if !exists || !found || s.operation.ID != operation || s.operation.Phase != domain.ReplacementStarting || s.operation.SuccessorSessionID != id || attempt.ID != attemptID || rec.Metadata.ControllerGeneration != "" || rec.Metadata.ProviderConversationID != "" || rec.IsTerminated {
		return false, nil
	}
	s.operation.SpawnAttempts[len(s.operation.SpawnAttempts)-1].FailedAt = &at
	s.operation.Revision++
	rec.ProvisionState, rec.ProvisionError = domain.SessionProvisionFailed, message
	s.sessions[id] = rec
	return true, nil
}

func (s *replacementSuccessorStore) CreateClientRequestSession(ctx context.Context, seed domain.SessionRecord) (domain.SessionRecord, bool, error) {
	s.creates++
	rec, fresh, err := s.fakeStore.CreateClientRequestSession(ctx, seed)
	if err == nil && fresh {
		s.operation.SuccessorSessionID = rec.ID
		s.operation.Revision++
	}
	return rec, fresh, err
}

func replacementSuccessorFixture(t *testing.T) (*Manager, *replacementSuccessorStore, *replacementPreflightChat, *[]func()) {
	t.Helper()
	chat := &replacementPreflightChat{recordingLauncher: &recordingLauncher{}}
	m, base, _ := newChatManager(chat)
	m.dataDir = t.TempDir()
	m.browserCapabilities = browsersvc.NewAuthority()
	project := base.projects["mer"]
	project.Path = t.TempDir()
	base.projects["mer"] = project
	var events []string
	m.SetAgentAccounts(&replacementPreflightAccounts{transitionAccounts: newTransitionAccounts(&events)})
	target, err := m.PreflightReplacementTarget(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	source := mkLive("mer-source")
	source.Kind, source.Harness, source.Mode = domain.KindOrchestrator, domain.HarnessClaudeCode, domain.SessionModeChat
	source.Metadata.ControllerGeneration, source.Metadata.ProviderConversationID, source.Metadata.BrowserCapabilityVerifier = "source-generation", "source-native", "source-verifier"
	source.Metadata.Branch, source.Metadata.WorkspacePath = DefaultOrchestratorBranch("mer", m.dataDir), t.TempDir()
	owner := source.ControllerOwner()
	source.IsTerminated = true
	base.sessions[source.ID] = source
	now := time.Now().UTC()
	fingerprint, err := domain.ReplacementProjectFingerprint(project)
	if err != nil {
		t.Fatal(err)
	}
	op := domain.OrchestratorReplacement{ID: "11111111-1111-4111-8111-111111111111", Revision: 4, ProjectID: source.ProjectID, ProjectFingerprint: fingerprint, SourceSessionID: source.ID, SourceOwner: owner, SourceCapabilityVerifier: source.Metadata.BrowserCapabilityVerifier, SourceRetiredAt: &now, Phase: domain.ReplacementStarting, SharedDir: filepath.Join(m.dataDir, "shared"), DocumentPath: "handoff.md", TargetAccountID: target.AccountID, TargetAgentConfig: target.AgentConfig, SpawnRequestID: "replacement:11111111-1111-4111-8111-111111111111:successor", Preparation: &domain.ReplacementPreparation{SubmittedHash: strings.Repeat("a", 64)}, Transcript: &domain.ReplacementTranscript{AttemptID: "22222222-2222-4222-8222-222222222222", TranscriptHash: strings.Repeat("b", 64), Receipt: &domain.HandoffEvidenceReceipt{Directory: filepath.Join(m.dataDir, "evidence"), ManifestHash: strings.Repeat("c", 64), InputHash: strings.Repeat("d", 64)}}}
	op.SpawnRequestHash, err = domain.ReplacementSpawnRequestHash(op)
	if err != nil {
		t.Fatal(err)
	}
	store := &replacementSuccessorStore{replacementFenceStore: &replacementFenceStore{fakeStore: base, operation: op}}
	m.store = store
	return m, store, chat, deferredBackground(m)
}

func TestReplacementSuccessorUsesOnePromptlessAsyncSpawnAndPreservesSource(t *testing.T) {
	m, store, chat, deferred := replacementSuccessorFixture(t)
	ctx := context.Background()
	op := store.operation
	source := store.sessions[op.SourceSessionID]
	path := filepath.Join(source.Metadata.WorkspacePath, "untracked.txt")
	if err := os.WriteFile(path, []byte("preserve retired source bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.Spawn(ctx, ports.SpawnConfig{ProjectID: op.ProjectID, Kind: domain.KindOrchestrator, ClientRequestID: op.SpawnRequestID, ClientRequestHash: op.SpawnRequestHash, RequestedMode: domain.SessionModeChat}); !errors.Is(err, domain.ErrReplacementInProgress) {
		t.Fatalf("public Spawn bypassed active operation: %v", err)
	}
	created, err := m.EnsureReplacementSuccessor(ctx, op)
	if err != nil || created.ID == "" || created.ID != store.operation.SuccessorSessionID || created.ProvisionState != domain.SessionProvisionProvisioning || created.ClientRequestID != op.SpawnRequestID || created.ClientRequestHash != op.SpawnRequestHash || created.Harness != domain.HarnessClaudeCode || created.Mode != domain.SessionModeChat {
		t.Fatalf("successor was not bound/provisioning: %+v err=%v", created, err)
	}
	again, err := m.EnsureReplacementSuccessor(ctx, store.operation)
	if err != nil || again.ID != created.ID || store.creates != 1 || len(*deferred) != 1 || len(chat.started) != 0 || len(chat.queued) != 0 || len(chat.turns) != 0 {
		t.Fatal("replayed intent started a second controller or premature prompt", err)
	}
	(*deferred)[0]()
	again, err = m.EnsureReplacementSuccessor(ctx, store.operation)
	if err != nil || again.ID != created.ID || len(chat.started) != 1 || store.creates != 1 || len(chat.queued) != 0 || len(chat.turns) != 0 {
		t.Fatal("ready successor was respawned or sent an unbound prompt", err)
	}
	if again.Metadata.Branch == source.Metadata.Branch || !reflect.DeepEqual(store.sessions[source.ID], source) || m.workspace.(*fakeWorkspace).destroyed != 0 || m.runtime.(*fakeRuntime).destroyed != 0 {
		t.Fatal("successor reused/destroyed retired source workspace")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "preserve retired source bytes" {
		t.Fatal("source bytes changed", err)
	}
}

type changingReplacementWorkspace struct {
	*fakeWorkspace
	change func()
}

func (w changingReplacementWorkspace) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	created, err := w.fakeWorkspace.Create(ctx, cfg)
	w.change()
	return created, err
}

func TestReplacementSuccessorRechecksReservationImmediatelyBeforeProvider(t *testing.T) {
	for _, change := range []string{"project during workspace creation", "operation no longer starting", "native already claimed"} {
		t.Run(change, func(t *testing.T) {
			m, store, chat, deferred := replacementSuccessorFixture(t)
			if change == "project during workspace creation" {
				m.workspace = changingReplacementWorkspace{fakeWorkspace: m.workspace.(*fakeWorkspace), change: func() {
					project := store.projects[string(store.operation.ProjectID)]
					project.Config.OrchestratorRules = "changed during worktree setup"
					store.projects[string(store.operation.ProjectID)] = project
				}}
			}
			rec, err := m.EnsureReplacementSuccessor(context.Background(), store.operation)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "operation no longer starting":
				store.operation.Phase = domain.ReplacementRecoveryRequired
			case "native already claimed":
				changed := store.sessions[rec.ID]
				changed.Metadata.ProviderConversationID = "other-native"
				store.sessions[rec.ID] = changed
				chat.live = true
			}
			(*deferred)[0]()
			if len(chat.started) != 0 || len(chat.queued) != 0 || len(chat.turns) != 0 {
				t.Fatal("stale background work started a provider")
			}
			if len(chat.stopped) != 0 {
				t.Fatal("stale prelaunch failure stopped a controller it did not launch")
			}
			if change == "native already claimed" {
				if store.sessions[rec.ID].ProvisionState != domain.SessionProvisionProvisioning || store.sessions[rec.ID].Metadata.ProviderConversationID != "other-native" {
					t.Fatal("stale background work overwrote the new owner's state")
				}
			} else if store.sessions[rec.ID].ProvisionState != domain.SessionProvisionFailed && change != "operation no longer starting" {
				t.Fatal("configuration failure did not remain visible")
			}
		})
	}
}

func TestReplacementSuccessorIncompleteSeedRequiresRecoveryWithoutFreshSpawn(t *testing.T) {
	for _, change := range []string{"failed", "terminated", "missing", "request incomplete", "background lost", "wrong account", "wrong hash", "wrong mode"} {
		t.Run(change, func(t *testing.T) {
			m, store, chat, deferred := replacementSuccessorFixture(t)
			rec, err := m.EnsureReplacementSuccessor(context.Background(), store.operation)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "failed":
				rec.ProvisionState = domain.SessionProvisionFailed
			case "terminated":
				rec.IsTerminated = true
			case "request incomplete", "background lost":
				rec.ClientRequestCommitted = change == "background lost"
				m.asyncChatSpawnsMu.Lock()
				delete(m.asyncChatSpawns, rec.ID)
				m.asyncChatSpawnsMu.Unlock()
			case "wrong account":
				if err := m.agentAccounts.CommitSession(context.Background(), rec.ID, domain.HarnessClaudeCode, "acct-a"); err != nil {
					t.Fatal(err)
				}
			case "wrong hash":
				rec.ClientRequestHash = strings.Repeat("e", 64)
			case "wrong mode":
				rec.Mode = domain.SessionModeTUI
			}
			store.sessions[rec.ID] = rec
			if change == "missing" {
				delete(store.sessions, rec.ID)
			}
			if _, err := m.EnsureReplacementSuccessor(context.Background(), store.operation); err == nil {
				t.Fatal("invalid bound seed silently replaced")
			}
			if store.creates != 1 || len(*deferred) != 1 || len(chat.started) != 0 || len(chat.queued) != 0 || len(chat.turns) != 0 {
				t.Fatal("incomplete seed caused another spawn or prompt")
			}
		})
	}
}

func TestReplacementSuccessorRefusesFutureRevisionOrChangedBindingBeforeSpawn(t *testing.T) {
	for _, change := range []string{"revision", "operation", "missing retirement", "source live", "source owner", "hash", "account", "config", "archived"} {
		t.Run(change, func(t *testing.T) {
			m, store, chat, deferred := replacementSuccessorFixture(t)
			op := store.operation
			switch change {
			case "revision":
				op.Revision++
			case "operation":
				op.ID = "wrong-operation"
			case "missing retirement":
				store.operation.SourceRetiredAt = nil
			case "source live":
				rec := store.sessions[op.SourceSessionID]
				rec.IsTerminated = false
				store.sessions[rec.ID] = rec
			case "source owner":
				rec := store.sessions[op.SourceSessionID]
				rec.Metadata.ControllerGeneration = "rotated"
				store.sessions[rec.ID] = rec
			case "hash":
				store.operation.SpawnRequestHash = strings.Repeat("e", 64)
			case "account":
				store.operation.TargetAccountID = "wrong-account"
			case "archived":
				project := store.projects[string(op.ProjectID)]
				project.ArchivedAt = time.Now()
				store.projects[string(op.ProjectID)] = project
			case "config":
				project := store.projects[string(op.ProjectID)]
				project.Config.OrchestratorRules = "changed"
				store.projects[string(op.ProjectID)] = project
			}
			if _, err := m.EnsureReplacementSuccessor(context.Background(), op); err == nil {
				t.Fatal("changed binding authorized a successor")
			}
			if store.creates != 0 || len(*deferred) != 0 || len(chat.started) != 0 || len(chat.queued) != 0 || len(chat.turns) != 0 {
				t.Fatal("stale binding reached spawn side effects")
			}
		})
	}
}

type changingReplacementPreflight struct {
	*replacementPreflightChat
	change func()
}

func (c changingReplacementPreflight) PreflightReplacementChat(ctx context.Context, harness domain.AgentHarness, permissions ports.PermissionMode) error {
	if err := c.replacementPreflightChat.PreflightReplacementChat(ctx, harness, permissions); err != nil {
		return err
	}
	c.change()
	return nil
}

func TestReplacementSuccessorRejectsProjectChangeAcrossPreflightAndBackground(t *testing.T) {
	for _, when := range []string{"after preflight", "before background launch"} {
		t.Run(when, func(t *testing.T) {
			m, store, chat, deferred := replacementSuccessorFixture(t)
			change := func() {
				project := store.projects[string(store.operation.ProjectID)]
				project.Config.OrchestratorRules = "new unpinned rules"
				store.projects[string(store.operation.ProjectID)] = project
			}
			if when == "after preflight" {
				m.chat = changingReplacementPreflight{replacementPreflightChat: chat, change: change}
			}
			rec, err := m.EnsureReplacementSuccessor(context.Background(), store.operation)
			if when == "after preflight" {
				if !errors.Is(err, domain.ErrReplacementConfigChanged) || store.creates != 0 || len(*deferred) != 0 {
					t.Fatalf("preflight gap accepted changed project: creates=%d bg=%d err=%v", store.creates, len(*deferred), err)
				}
			} else {
				if err != nil || len(*deferred) != 1 {
					t.Fatal(err)
				}
				change()
				(*deferred)[0]()
				if store.sessions[rec.ID].ProvisionState != domain.SessionProvisionFailed {
					t.Fatal("changed project launched successor instead of failing visibly")
				}
			}
			if len(chat.started) != 0 || len(chat.queued) != 0 || len(chat.turns) != 0 {
				t.Fatal("changed configuration reached provider")
			}
		})
	}
}

func (s *replacementSuccessorStore) WithReplacementSpawnPublication(ctx context.Context, operation string, id domain.SessionID, attemptID string, publish func(context.Context) error) error {
	a, ok := s.operation.LatestSpawnAttempt()
	if !ok || s.operation.ID != operation || s.operation.SuccessorSessionID != id || a.ID != attemptID || a.RetryGeneration != s.operation.SpawnRetryGeneration || a.FailedAt != nil {
		return domain.ErrReplacementStateInvalid
	}
	return publish(ctx)
}
