package orchestratorreplacement

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func (s *Service) startSuccessor(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	if err := s.checkProject(ctx, op); err != nil {
		return op, err
	}
	if op.SourceRetiredAt == nil {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_SPAWN_INTENT_INVALID", "Source retirement must precede successor creation", nil)
	}
	if !s.deps.Now().Before(op.DeadlineAt) {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_SUCCESSOR_START_TIMEOUT", "Successor did not become verified before the operation deadline", nil)
	}
	hash, err := domain.ReplacementSpawnRequestHash(op)
	if err != nil {
		return op, err
	}
	requestID := "replacement:" + op.ID + ":successor"
	bootstrapID := "replacement:" + op.ID + ":bootstrap"
	probeID := "replacement:" + op.ID + ":probe"
	if op.SpawnRequestID == "" && op.SpawnRequestHash == "" && op.BootstrapClientID == "" && op.ResumeProbeClientID == "" && op.SuccessorSessionID == "" {
		op.SpawnRequestID, op.SpawnRequestHash = requestID, hash
		op.BootstrapClientID, op.ResumeProbeClientID = bootstrapID, probeID
		return s.save(ctx, op)
	}
	if op.SpawnRequestID != requestID || op.SpawnRequestHash != hash || op.BootstrapClientID != bootstrapID || op.ResumeProbeClientID != probeID {
		return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_SPAWN_INTENT_INVALID", "Partial or changed spawn intent requires recovery; another identity must not be allocated", nil)
	}
	started, actionErr := s.deps.Runtime.EnsureReplacementSuccessor(ctx, op)
	// Session INSERT atomically binds the ID and advances revision. Always read
	// that fact, including when the caller lost the seed's return value.
	current, err := s.Get(ctx, op.ID)
	if err != nil {
		return op, err
	}
	if actionErr != nil {
		return current, actionErr
	}
	if current.SuccessorSessionID == "" || started.ID != current.SuccessorSessionID {
		return current, apierr.Conflict("ORCHESTRATOR_HANDOFF_SUCCESSOR_UNVERIFIED", "Runtime did not return the durably bound successor", nil)
	}
	seed, found, err := s.deps.Store.GetSession(ctx, current.SuccessorSessionID)
	if err != nil {
		return current, err
	}
	if !found || seed.ProjectID != op.ProjectID || seed.Kind != domain.KindOrchestrator || seed.Harness != domain.HarnessClaudeCode || domain.NormalizeSessionMode(seed.Mode) != domain.SessionModeChat || seed.ClientRequestID != op.SpawnRequestID || seed.ClientRequestHash != op.SpawnRequestHash {
		return current, apierr.Conflict("ORCHESTRATOR_HANDOFF_SUCCESSOR_UNVERIFIED", "Bound successor facts differ from the pinned spawn intent", nil)
	}
	if seed.IsTerminated || seed.ProvisionState == domain.SessionProvisionFailed {
		return current, apierr.Conflict("ORCHESTRATOR_HANDOFF_SUCCESSOR_RECOVERY_REQUIRED", "The one bound successor requires recovery", nil)
	}
	if seed.ProvisionState != domain.SessionProvisionReady {
		return current, nil
	}
	proof := current.SuccessorController
	if proof == nil || proof.CommittedAt.IsZero() || proof.CapabilityVerifier == "" || seed.ControllerOwner() != proof.Owner || seed.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier {
		return current, apierr.Conflict("ORCHESTRATOR_HANDOFF_SUCCESSOR_UNVERIFIED", "Ready successor lacks its committed initial controller receipt", nil)
	}
	observation, err := s.deps.Runtime.ObserveReplacementSuccessor(ctx, current)
	if err != nil {
		return current, err
	}
	if observation.Owner != proof.Owner || observation.NativeID == "" || observation.NativeID != proof.Owner.ProviderConversationID || observation.Provenance != domain.NativeThreadMain || observation.ObservedAt.IsZero() {
		return current, apierr.Conflict("ORCHESTRATOR_HANDOFF_SUCCESSOR_UNVERIFIED", "Successor controller identity is not positively verified", nil)
	}
	if observation.State != ports.ChatControllerReady || observation.PendingApproval || observation.ActiveTurn != nil && !observation.ActiveTurn.State.Terminal() {
		return current, nil
	}
	// This certifies initial readiness only. Bootstrap, same-native resume and
	// writer activation each require their own durable evidence.
	current.Phase, current.ResumePhase, current.Error = domain.ReplacementControllerReady, "", nil
	return s.save(ctx, current)
}
