package sessionmanager

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// EnsureReplacementProbe dispatches only under the resumed controller receipt.
func (m *Manager) EnsureReplacementProbe(ctx context.Context, op domain.OrchestratorReplacement, prompt string, allowNew bool) (ports.ReplacementPreparationDelivery, error) {
	var zero ports.ReplacementPreparationDelivery
	ctx, release, err := m.acquireOrchestratorGate(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	defer release()
	if op.Phase != domain.ReplacementVerifyingResume || op.ResumeEvidence == nil || op.ResumeEvidence.ResumedAt == nil || op.ResumeEvidence.Probe == nil {
		return zero, domain.ErrReplacementStateInvalid
	}
	if _, err := m.ObserveReplacementSuccessor(ctx, op); err != nil {
		return zero, err
	}
	chat, ok := m.chat.(interface {
		EnsureReplacementChatProbe(context.Context, domain.OrchestratorReplacement, string, bool) (ports.ReplacementPreparationDelivery, error)
	})
	if !ok {
		return zero, ports.ErrChatUnsupported
	}
	return chat.EnsureReplacementChatProbe(ctx, op, prompt, allowNew)
}

// QuiesceReplacementSuccessor stops only the operation's verified Chat owner.
func (m *Manager) QuiesceReplacementSuccessor(ctx context.Context, op domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error) {
	var zero domain.ReplacementControllerStop
	ctx, release, err := m.acquireOrchestratorGate(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	defer release()
	if op.Phase != domain.ReplacementVerifyingResume {
		return zero, domain.ErrReplacementStateInvalid
	}
	reader, ok := m.store.(replacementAdmissionReader)
	if !ok {
		return zero, domain.ErrReplacementStateInvalid
	}
	current, found, err := reader.GetActiveOrchestratorReplacement(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	if !found || current.ID != op.ID || current.Revision != op.Revision || current.Phase != op.Phase {
		return zero, domain.ErrReplacementStateInvalid
	}
	rec, found, err := m.store.GetSession(ctx, current.SuccessorSessionID)
	if err != nil {
		return zero, err
	}
	if !found || !current.AllowsChatQuiescence(rec) {
		return zero, domain.ErrReplacementStateInvalid
	}
	if _, err := m.ObserveReplacementSuccessor(ctx, op); err != nil {
		return zero, err
	}
	chat, ok := m.chat.(interface {
		QuiesceReplacementChatSuccessor(context.Context, domain.OrchestratorReplacement) (domain.ReplacementControllerStop, error)
	})
	if !ok {
		return zero, ports.ErrChatUnsupported
	}
	return chat.QuiesceReplacementChatSuccessor(ctx, op)
}

// EnsureReplacementBootstrap shares the lifecycle reservation with readonly
// successor admission; the Chat service separately verifies exact prompt intent.
func (m *Manager) EnsureReplacementBootstrap(ctx context.Context, op domain.OrchestratorReplacement, prompt string, allowNew bool) (ports.ReplacementPreparationDelivery, error) {
	var zero ports.ReplacementPreparationDelivery
	ctx, release, err := m.acquireOrchestratorGate(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	defer release()
	if op.Phase != domain.ReplacementControllerReady || op.Bootstrap == nil {
		return zero, domain.ErrReplacementStateInvalid
	}
	if _, err := m.ObserveReplacementSuccessor(ctx, op); err != nil {
		return zero, err
	}
	chat, ok := m.chat.(interface {
		EnsureReplacementChatBootstrap(context.Context, domain.OrchestratorReplacement, string, bool) (ports.ReplacementPreparationDelivery, error)
	})
	if !ok {
		return zero, ports.ErrChatUnsupported
	}
	return chat.EnsureReplacementChatBootstrap(ctx, op, prompt, allowNew)
}

// ObserveReplacementSuccessor holds the lifecycle reservation while checking
// the one bound target. It never recovers, stops or sends to a controller.
func (m *Manager) ObserveReplacementSuccessor(ctx context.Context, expected domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error) {
	var zero ports.ReplacementSourceObservation
	ctx, release, err := m.acquireOrchestratorGate(ctx, expected.ProjectID)
	if err != nil {
		return zero, err
	}
	defer release()
	reader, ok := m.store.(replacementAdmissionReader)
	if !ok {
		return zero, domain.ErrReplacementStateInvalid
	}
	op, found, err := reader.GetActiveOrchestratorReplacement(ctx, expected.ProjectID)
	if err != nil {
		return zero, err
	}
	if !found || op.ID != expected.ID || op.Revision != expected.Revision || op.Phase != expected.Phase || op.SourceRetiredAt == nil || op.SuccessorSessionID == "" || op.SuccessorSessionID != expected.SuccessorSessionID || op.SpawnRequestID != expected.SpawnRequestID || op.SpawnRequestHash != expected.SpawnRequestHash {
		return zero, domain.ErrReplacementStateInvalid
	}
	rec, found, err := m.store.GetSession(ctx, op.SuccessorSessionID)
	if err != nil {
		return zero, err
	}
	if !found || rec.IsTerminated || rec.ProjectID != op.ProjectID || rec.Kind != domain.KindOrchestrator || rec.Harness != domain.HarnessClaudeCode || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat || rec.ClientRequestID != op.SpawnRequestID || rec.ClientRequestHash != op.SpawnRequestHash {
		return zero, domain.ErrReplacementStateInvalid
	}
	if rec.ProvisionState != domain.SessionProvisionProvisioning || op.Phase != domain.ReplacementStarting {
		proof, verified := op.ExpectedSuccessorController()
		if op.ActivatedSuccessorMatches(rec) {
			matches := false
			if checker, ok := m.store.(interface {
				ReplacementActivatedOwner(context.Context, string, domain.SessionRecord) (bool, error)
			}); ok {
				var err error
				matches, err = checker.ReplacementActivatedOwner(ctx, op.ID, rec)
				if err != nil {
					return zero, err
				}
			} else if rec.ControllerOwner() == op.WriterActivation.Owner && rec.Metadata.BrowserCapabilityVerifier == op.WriterActivation.CapabilityVerifier {
				if fence, ok := m.store.(interface {
					ReplacementWakeFenced(context.Context, domain.SessionID) (bool, error)
				}); ok {
					fenced, err := fence.ReplacementWakeFenced(ctx, rec.ID)
					if err != nil {
						return zero, err
					}
					matches = !fenced
				}
			}
			if !matches {
				return zero, domain.ErrReplacementIntakeFenced
			}
			proof.Owner, proof.CapabilityVerifier, verified = rec.ControllerOwner(), rec.Metadata.BrowserCapabilityVerifier, true
		}
		if rec.ProvisionState != domain.SessionProvisionReady || !verified || rec.ControllerOwner() != proof.Owner || rec.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier {
			return zero, domain.ErrReplacementStateInvalid
		}
	}
	chat, ok := m.chat.(interface {
		ObserveReplacementChatSuccessor(context.Context, domain.OrchestratorReplacement) (ports.ReplacementSourceObservation, error)
	})
	if !ok {
		return zero, ports.ErrChatUnsupported
	}
	return chat.ObserveReplacementChatSuccessor(ctx, op)
}

// EnsureReplacementSuccessor reuses only the operation's one durable seed. A
// fresh controller starts without a user turn; the coordinator separately binds
// the first handoff prompt after readiness. Ordinary Spawn remains fenced.
func (m *Manager) EnsureReplacementSuccessor(ctx context.Context, expected domain.OrchestratorReplacement) (domain.SessionRecord, error) {
	var zero domain.SessionRecord
	ctx, release, err := m.acquireOrchestratorGate(ctx, expected.ProjectID)
	if err != nil {
		return zero, err
	}
	defer release()
	reader, ok := m.store.(replacementAdmissionReader)
	if !ok {
		return zero, domain.ErrReplacementStateInvalid
	}
	op, found, err := reader.GetActiveOrchestratorReplacement(ctx, expected.ProjectID)
	if err != nil {
		return zero, err
	}
	// Background launch/controller receipts can advance revision while this
	// caller waits for the gate. Revalidate the same pinned identity and use
	// the committed row; progress alone is not source ownership loss.
	if !found || op.ID != expected.ID || op.Revision < expected.Revision || op.Phase != domain.ReplacementStarting || op.SourceRetiredAt == nil || op.SourceSessionID != expected.SourceSessionID || op.SourceOwner != expected.SourceOwner || op.SourceCapabilityVerifier != expected.SourceCapabilityVerifier || op.SpawnRequestID == "" || op.SpawnRequestID != expected.SpawnRequestID || op.SpawnRequestHash != expected.SpawnRequestHash || op.ProjectFingerprint != expected.ProjectFingerprint {
		return zero, domain.ErrReplacementSourceChanged
	}
	hash, err := domain.ReplacementSpawnRequestHash(op)
	if err != nil || hash != op.SpawnRequestHash {
		return zero, domain.ErrReplacementStateInvalid
	}
	source, found, err := m.store.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		return zero, err
	}
	retiredOwner := op.SourceOwner
	retiredOwner.IsTerminated = true
	if !found || source.ProjectID != op.ProjectID || source.Kind != domain.KindOrchestrator || source.ControllerOwner() != retiredOwner || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
		return zero, domain.ErrReplacementSourceChanged
	}
	project, err := m.loadProject(ctx, op.ProjectID)
	if err != nil {
		return zero, err
	}
	if !project.ArchivedAt.IsZero() {
		return zero, domain.ErrReplacementConfigChanged
	}
	fingerprint, err := domain.ReplacementProjectFingerprint(project)
	if err != nil {
		return zero, err
	}
	if fingerprint != op.ProjectFingerprint {
		return zero, domain.ErrReplacementConfigChanged
	}
	if existing, found, err := m.store.GetSessionByClientRequestID(ctx, op.SpawnRequestID); err != nil {
		return zero, err
	} else if found {
		return m.existingReplacementSuccessor(ctx, op, existing)
	}
	if op.SuccessorSessionID != "" {
		return zero, domain.ErrReplacementSourceChanged
	}
	target, err := m.PreflightReplacementTarget(ctx, project)
	if err != nil {
		return zero, err
	}
	if target.AccountID != op.TargetAccountID || target.AgentConfig != op.TargetAgentConfig {
		return zero, domain.ErrReplacementConfigChanged
	}
	branch := m.replacementSuccessorBranch(project, op)
	cfg := ports.SpawnConfig{ProjectID: op.ProjectID, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, RequestedMode: domain.SessionModeChat, Async: true, ClientRequestID: op.SpawnRequestID, ClientRequestHash: op.SpawnRequestHash, AgentConfig: op.TargetAgentConfig, AgentConfigResolved: true, AgentAccountID: op.TargetAccountID, Branch: branch}
	rec, _, _, err := m.spawn(ctx, cfg, &op)
	return rec, err
}

func (m *Manager) replacementSuccessorBranch(project domain.ProjectRecord, op domain.OrchestratorReplacement) string {
	if project.Kind.WithDefault() == domain.ProjectKindScratch {
		return ""
	}
	return aoBranch(generatedBranchNamespace(m.dataDir), sessionPrefix(project)+"-orchestrator-replacement-"+op.ID)
}

func (m *Manager) checkReplacementSpawnReservation(ctx context.Context, expected domain.OrchestratorReplacement, id domain.SessionID) error {
	reader, ok := m.store.(replacementAdmissionReader)
	if !ok {
		return domain.ErrReplacementStateInvalid
	}
	op, found, err := reader.GetActiveOrchestratorReplacement(ctx, expected.ProjectID)
	if err != nil {
		return err
	}
	if !found || op.ID != expected.ID || op.Phase != domain.ReplacementStarting || op.SourceRetiredAt == nil || op.SuccessorSessionID != id || op.SpawnRequestID != expected.SpawnRequestID || op.SpawnRequestHash != expected.SpawnRequestHash || op.ProjectFingerprint != expected.ProjectFingerprint {
		return domain.ErrReplacementSourceChanged
	}
	attempt, exists := op.LatestSpawnAttempt()
	expectedAttempt, expectedExists := expected.LatestSpawnAttempt()
	if expectedExists && (!exists || attempt.ID != expectedAttempt.ID || attempt.RetryGeneration != op.SpawnRetryGeneration || attempt.LaunchIntentAt != nil || attempt.FailedAt != nil) || !expectedExists && exists {
		return domain.ErrReplacementStateInvalid
	}

	project, err := m.loadProject(ctx, op.ProjectID)
	if err != nil {
		return err
	}
	if !project.ArchivedAt.IsZero() {
		return domain.ErrReplacementConfigChanged
	}
	fingerprint, err := domain.ReplacementProjectFingerprint(project)
	if err != nil {
		return err
	}
	if fingerprint != op.ProjectFingerprint {
		return domain.ErrReplacementConfigChanged
	}
	rec, found, err := m.store.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if !found || rec.IsTerminated || rec.ProjectID != op.ProjectID || rec.Kind != domain.KindOrchestrator || rec.Harness != domain.HarnessClaudeCode || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat || rec.ClientRequestID != op.SpawnRequestID || rec.ClientRequestHash != op.SpawnRequestHash || rec.Metadata.ControllerGeneration != "" || rec.Metadata.ProviderConversationID != "" {
		return domain.ErrReplacementSourceChanged
	}
	if m.agentAccounts == nil {
		return ports.ErrAgentAccountUnknown
	}
	account, err := m.agentAccounts.SessionAccountID(ctx, id, domain.HarnessClaudeCode)
	if err != nil {
		return err
	}
	if account != op.TargetAccountID {
		return domain.ErrReplacementConfigChanged
	}
	return ctx.Err()
}

func (m *Manager) existingReplacementSuccessor(ctx context.Context, op domain.OrchestratorReplacement, rec domain.SessionRecord) (domain.SessionRecord, error) {
	if rec.ID != op.SuccessorSessionID || rec.ProjectID != op.ProjectID || rec.Kind != domain.KindOrchestrator || rec.Harness != domain.HarnessClaudeCode || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeChat || rec.ClientRequestID != op.SpawnRequestID || rec.ClientRequestHash != op.SpawnRequestHash {
		return rec, domain.ErrReplacementSourceChanged
	}
	if rec.Metadata.Model != op.TargetAgentConfig.Model || rec.Metadata.Effort != op.TargetAgentConfig.Effort || rec.Metadata.Permissions != op.TargetAgentConfig.Permissions {
		return rec, domain.ErrReplacementConfigChanged
	}
	if m.agentAccounts == nil {
		return rec, ports.ErrAgentAccountUnknown
	}
	account, err := m.agentAccounts.SessionAccountID(ctx, rec.ID, domain.HarnessClaudeCode)
	if err != nil {
		return rec, err
	}
	if account != op.TargetAccountID {
		return rec, domain.ErrReplacementConfigChanged
	}
	if rec.IsTerminated {
		return rec, successorRecoveryRequired()
	}
	m.asyncChatSpawnsMu.Lock()
	_, running := m.asyncChatSpawns[rec.ID]
	m.asyncChatSpawnsMu.Unlock()
	if rec.ProvisionState == domain.SessionProvisionFailed || ((!rec.ClientRequestCommitted || rec.ProvisionState.IsProvisioning()) && !running) {
		if running {
			return rec, successorRecoveryRequired()
		}
		return m.retryReplacementSuccessor(ctx, op, rec)
	}

	return rec, nil
}

func successorRecoveryRequired() error {
	return &ports.NativeTranscriptError{Code: "ORCHESTRATOR_HANDOFF_SUCCESSOR_RECOVERY_REQUIRED", Reason: "The bound successor requires recovery; another session must not be created"}
}
