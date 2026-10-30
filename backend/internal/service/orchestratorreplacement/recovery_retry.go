package orchestratorreplacement

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// retryRetainedBoundary restores only reconciliation. Existing intents force
// their ordinary lookup-only recovery paths; no receipt, seed or native owner
// is reset, and no provider action is performed by Retry itself.
func (s *Service) retryRetainedBoundary(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	refuse := func() (domain.OrchestratorReplacement, error) {
		return op, apierr.Conflict("ORCHESTRATOR_REPLACEMENT_RECOVERY_REQUIRED", "This boundary requires verification of the retained operation, evidence and native controller; no fresh replacement is allowed", nil)
	}
	if err := s.checkProject(ctx, op); err != nil {
		return op, err
	}
	phase := op.ResumePhase
	if op.SourceRetiredAt == nil {
		switch phase {
		case domain.ReplacementExportingTranscript, domain.ReplacementTranscriptVerified, domain.ReplacementRetiring:
		default:
			return refuse()
		}
		if op.SourceStopIntentAt == nil || op.FrozenAt == nil || op.Preparation == nil || op.Preparation.VerifiedAt == nil || op.Transcript == nil || op.SuccessorSessionID != "" {
			return refuse()
		}
		if phase != domain.ReplacementExportingTranscript && (op.SourceStop == nil || op.Transcript.Receipt == nil) {
			return refuse()
		}
	} else {
		switch phase {
		case domain.ReplacementStarting, domain.ReplacementControllerReady, domain.ReplacementHandoffSettled, domain.ReplacementVerifyingResume, domain.ReplacementActivatingSharedWriter, domain.ReplacementNotifying:
		default:
			return refuse()
		}
		if op.SourceStop == nil || op.Transcript == nil || op.Transcript.Receipt == nil {
			return refuse()
		}
		if phase == domain.ReplacementHandoffSettled && (op.Bootstrap == nil || op.Bootstrap.SettledAt == nil) {
			return refuse()
		}
		if phase == domain.ReplacementVerifyingResume && (op.ResumeEvidence == nil || op.ResumeEvidence.StartedAt.IsZero()) {
			return refuse()
		}
		if phase == domain.ReplacementActivatingSharedWriter && (op.ResumeEvidence == nil || op.ResumeEvidence.VerifiedAt == nil) {
			return refuse()
		}
		if phase == domain.ReplacementNotifying && op.WriterActivation == nil {
			return refuse()
		}
	}
	source, found, err := s.deps.Store.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		return op, err
	}
	expected := op.SourceOwner
	expected.IsTerminated = op.SourceRetiredAt != nil
	if !found || source.ProjectID != op.ProjectID || source.ControllerOwner() != expected || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
		return refuse()
	}
	if op.SourceRetiredAt == nil && op.SourceStop != nil && !op.SourceStop.MatchesSource(source) {
		return refuse()
	}
	candidate := op
	candidate.Phase = phase
	if candidate.SuccessorSessionID != "" {
		target, found, err := s.deps.Store.GetSession(ctx, candidate.SuccessorSessionID)
		if err != nil {
			return op, err
		}
		if !found || target.ProjectID != op.ProjectID || target.Kind != domain.KindOrchestrator || target.Harness != domain.HarnessClaudeCode || domain.NormalizeSessionMode(target.Mode) != domain.SessionModeChat || target.IsTerminated || target.ClientRequestID != op.SpawnRequestID || target.ClientRequestHash != op.SpawnRequestHash || phase != domain.ReplacementNotifying && (target.Metadata.Model != op.TargetAgentConfig.Model || target.Metadata.Effort != op.TargetAgentConfig.Effort || target.Metadata.Permissions != op.TargetAgentConfig.Permissions) {
			return refuse()
		}
		proof, verified := candidate.ExpectedSuccessorController()
		if verified {
			if (target.ControllerOwner() != proof.Owner || target.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier) && !candidate.ActivatedSuccessorMatches(target) {
				return refuse()
			}
		} else if phase != domain.ReplacementStarting || candidate.SuccessorController != nil || target.Metadata.ControllerGeneration != "" || target.Metadata.ProviderConversationID != "" {
			return refuse()
		}
	} else if phase != domain.ReplacementStarting && op.SourceRetiredAt != nil {
		return refuse()
	}
	now := s.deps.Now().UTC()
	next, applied, err := s.deps.Store.RetryRetainedReplacement(ctx, op.ID, op.Revision, now, now.Add(operationDeadline))
	if err != nil {
		return op, checkpointCapacityError(err)
	}
	if !applied {
		return op, errRevisionChanged
	}
	return next, nil
}
