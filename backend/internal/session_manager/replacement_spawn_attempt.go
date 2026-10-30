package sessionmanager

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type replacementSpawnAttemptStore interface {
	BeginReplacementSpawnAttempt(context.Context, string, domain.SessionID, string, time.Time) (domain.OrchestratorReplacement, domain.SessionRecord, error)
	MarkReplacementSpawnLaunch(context.Context, string, domain.SessionID, string, time.Time) (bool, error)
	FailReplacementSpawnAttempt(context.Context, string, domain.SessionID, string, string, time.Time) (bool, error)
}

func (m *Manager) markReplacementSpawnLaunch(ctx context.Context, op domain.OrchestratorReplacement, id domain.SessionID) error {
	attempt, exists := op.LatestSpawnAttempt()
	store, ok := m.store.(replacementSpawnAttemptStore)
	if !exists || !ok {
		return domain.ErrReplacementStateInvalid
	}
	marked, err := store.MarkReplacementSpawnLaunch(ctx, op.ID, id, attempt.ID, m.clock())
	if err != nil {
		return err
	}
	if !marked {
		return successorRecoveryRequired()
	}
	return nil
}

func (m *Manager) failAsyncChatSpawnForAttempt(ctx context.Context, in asyncChatSpawn, cause error) {
	if in.replacement == nil {
		m.failAsyncChatSpawn(ctx, in.record.ID, cause)
		return
	}
	op := *in.replacement
	attempt, exists := op.LatestSpawnAttempt()
	store, ok := m.store.(replacementSpawnAttemptStore)
	if !exists || !ok {
		return
	}
	m.logger.Error("replacement successor preparation failed", "sessionID", in.record.ID, "operationID", op.ID, "error", cause)
	cleanup, cancel := spawnRollbackContext(ctx)
	defer cancel()
	if _, err := store.FailReplacementSpawnAttempt(cleanup, op.ID, in.record.ID, attempt.ID, cause.Error(), m.clock()); err != nil {
		m.logger.Error("record replacement successor preparation failure", "sessionID", in.record.ID, "operationID", op.ID, "error", err)
	}
}

func (m *Manager) retryReplacementSuccessor(ctx context.Context, op domain.OrchestratorReplacement, rec domain.SessionRecord) (domain.SessionRecord, error) {
	attempt, exists := op.LatestSpawnAttempt()
	if !exists || attempt.LaunchIntentAt != nil || op.SpawnRetryGeneration <= attempt.RetryGeneration || rec.Metadata.ControllerGeneration != "" || rec.Metadata.ProviderConversationID != "" || m.chat.HasLiveChatController(rec.ID) {
		return rec, successorRecoveryRequired()
	}
	if m.agentAccounts == nil {
		return rec, ports.ErrAgentAccountUnknown
	}
	selected, err := m.agentAccounts.SessionAccountBinding(ctx, rec.ID, rec.Harness)
	if err != nil {
		return rec, err
	}
	if selected.AccountID != op.TargetAccountID {
		return rec, domain.ErrReplacementConfigChanged
	}
	ctx = ports.WithAgentAccount(ctx, selected)
	releaseHarness, err := m.beginHarnessUse(rec.Harness)
	if err != nil {
		return rec, err
	}
	defer func() {
		if releaseHarness != nil {
			releaseHarness()
		}
	}()
	if err := m.beginAgentResume(ctx, rec.ID); err != nil {
		return rec, err
	}
	scheduled := false
	defer func() {
		if !scheduled {
			m.endAgentResume(rec.ID)
		}
	}()
	project, err := m.loadProject(ctx, op.ProjectID)
	if err != nil {
		return rec, err
	}
	systemPrompt, err := m.buildSystemPrompt(ctx, rec.Kind, rec.ProjectID, rec.ID, rec.RequestedOutput, true)
	if err != nil {
		return rec, err
	}
	systemPromptFile, err := m.prepareSystemPromptFile(rec.ID, rec.Harness, systemPrompt)
	if err != nil {
		return rec, err
	}
	cfg := ports.SpawnConfig{ProjectID: op.ProjectID, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, RequestedMode: domain.SessionModeChat, Async: true, ClientRequestID: op.SpawnRequestID, ClientRequestHash: op.SpawnRequestHash, AgentConfig: op.TargetAgentConfig, AgentConfigResolved: true, AgentAccountID: op.TargetAccountID, Branch: rec.Metadata.Branch}
	retried, _, _, err := m.beginAsyncChatSpawn(ctx, asyncChatSpawn{replacement: &op, cfg: cfg, project: project, projectKind: projectKindForSession(project, rec.ProjectID), record: rec, branch: rec.Metadata.Branch, systemPrompt: systemPrompt, systemPromptFile: systemPromptFile, retry: true, releaseHarness: releaseHarness})
	if err != nil {
		return rec, err
	}
	scheduled, releaseHarness = true, nil
	return retried, nil
}

// The store holds the reservation only while this callback publishes DB facts.
func (m *Manager) publishReplacementSpawn(ctx context.Context, op *domain.OrchestratorReplacement, id domain.SessionID, publish func(context.Context) error) error {
	if op == nil {
		return publish(ctx)
	}
	attempt, ok := op.LatestSpawnAttempt()
	if !ok {
		return domain.ErrReplacementStateInvalid
	}
	store, ok := m.store.(interface {
		WithReplacementSpawnPublication(context.Context, string, domain.SessionID, string, func(context.Context) error) error
	})
	if !ok {
		return domain.ErrReplacementStateInvalid
	}
	return store.WithReplacementSpawnPublication(ctx, op.ID, id, attempt.ID, publish)
}
