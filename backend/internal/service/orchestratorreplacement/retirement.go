package orchestratorreplacement

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

func (s *Service) preflightRetirement(ctx context.Context, op domain.OrchestratorReplacement) error {
	if op.Transcript == nil || op.Transcript.Receipt == nil || op.Transcript.CapturedAt == nil || op.Transcript.TranscriptHash == "" || op.SourceStop == nil || op.FrozenAt == nil || op.SourceRetiredAt != nil {
		return apierr.Conflict("HANDOFF_EVIDENCE_UNVERIFIED", "Frozen, stopped, published source evidence is required before retirement", nil)
	}
	project, found, err := s.deps.Store.GetProject(ctx, string(op.ProjectID))
	if err != nil {
		return err
	}
	if !found {
		return apierr.Conflict("ORCHESTRATOR_REPLACEMENT_CONFIG_CHANGED", "Project is no longer available", nil)
	}
	// Re-resolve readiness, never replace the account/model pinned at acceptance.
	target, err := s.deps.Runtime.PreflightReplacementTarget(ctx, project)
	if err != nil || target.AccountID == "" {
		return apierr.Conflict("ORCHESTRATOR_HANDOFF_TARGET_UNAVAILABLE", "Pinned Claude Code target is not ready for native resume", nil)
	}
	if target.AccountID != op.TargetAccountID || target.AgentConfig != op.TargetAgentConfig {
		return apierr.Conflict("ORCHESTRATOR_HANDOFF_TARGET_CHANGED", "Target selection changed after acceptance", nil)
	}
	return s.checkSource(ctx, op)
}

func (s *Service) retire(ctx context.Context, op domain.OrchestratorReplacement) (domain.OrchestratorReplacement, error) {
	if err := s.preflightRetirement(ctx, op); err != nil {
		return op, err
	}
	snapshot, err := s.deps.Store.LoadTranscriptSourceSnapshot(ctx, op.SourceSessionID)
	if err != nil {
		return op, err
	}
	if !op.SourceStop.MatchesSource(snapshot.Session) || snapshot.Fingerprint != op.Transcript.SnapshotFingerprint {
		return op, apierr.Conflict("HANDOFF_EVIDENCE_SOURCE_CHANGED", "Source snapshot or confirmed shutdown changed before retirement", nil)
	}
	out, err := transcriptexport.Build(ctx, snapshot, op.Transcript.NativeCutoffs, s.deps.NativeTranscripts)
	if err != nil {
		return op, err
	}
	proof, err := s.deps.Documents.PrepareReplacementRetirement(ctx, op.ID, op.Revision, out)
	if err != nil {
		return op, err
	}
	_, actionErr := s.deps.Runtime.RetireReplacementSource(ctx, op, proof)
	// An atomic commit may succeed while its caller loses the response. Durable
	// facts win over either a runtime return value or a replay of the side effect.
	current, err := s.Get(ctx, op.ID)
	if err != nil {
		return op, err
	}
	if current.SourceRetiredAt != nil {
		source, found, err := s.deps.Store.GetSession(ctx, op.SourceSessionID)
		if err != nil {
			return current, err
		}
		retiredOwner := op.SourceOwner
		retiredOwner.IsTerminated = true
		if !found || source.ProjectID != op.ProjectID || source.Kind != domain.KindOrchestrator || source.ControllerOwner() != retiredOwner || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
			return current, apierr.Conflict("ORCHESTRATOR_HANDOFF_RETIREMENT_UNVERIFIED", "Retirement facts disagree with source state", nil)
		}
		return current, nil
	}
	if current.Revision != op.Revision {
		return current, nil
	}
	if actionErr != nil {
		return op, actionErr
	}
	return op, apierr.Conflict("ORCHESTRATOR_HANDOFF_RETIREMENT_UNVERIFIED", "Runtime did not durably commit source retirement", nil)
}
