package store

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func validateReplacementStage(op domain.OrchestratorReplacement) error {
	invalid := domain.ErrReplacementStateInvalid
	switch op.Phase {
	case domain.ReplacementAccepted, domain.ReplacementPreflight, domain.ReplacementWaitingSourceIdle:
		if op.Preparation != nil || op.Transcript != nil || op.FrozenAt != nil || op.SourceStopIntentAt != nil || op.SourceStop != nil || op.SourceRetiredAt != nil {
			return invalid
		}
	case domain.ReplacementAdoptingSharedFiles:
		if op.Preparation == nil {
			return invalid
		}
	case domain.ReplacementPreparingHandoff:
		if op.Preparation == nil || op.Preparation.LeaseEpoch <= 0 {
			return invalid
		}
	case domain.ReplacementHandoffVerified, domain.ReplacementFreezingSharedWrites:
		if op.Preparation == nil || op.Preparation.VerifiedAt == nil || op.Preparation.SubmittedAt == nil || op.Preparation.WriteReceiptID == "" || op.Preparation.ControlTurnID == "" {
			return invalid
		}
	case domain.ReplacementExportingTranscript, domain.ReplacementTranscriptVerified, domain.ReplacementRetiring, domain.ReplacementStarting, domain.ReplacementControllerReady, domain.ReplacementHandoffSettled, domain.ReplacementVerifyingResume, domain.ReplacementActivatingSharedWriter, domain.ReplacementNotifying, domain.ReplacementCompleted:
		if op.Preparation == nil || op.Preparation.VerifiedAt == nil || op.FrozenAt == nil || op.Transcript == nil {
			return invalid
		}
		if op.Phase != domain.ReplacementExportingTranscript && (op.SourceStop == nil || op.Transcript.Receipt == nil) {
			return invalid
		}
		if op.Phase != domain.ReplacementExportingTranscript && op.Phase != domain.ReplacementTranscriptVerified && op.Phase != domain.ReplacementRetiring && op.SourceRetiredAt == nil {
			return invalid
		}
	}
	switch op.Phase {
	case domain.ReplacementControllerReady, domain.ReplacementHandoffSettled, domain.ReplacementVerifyingResume, domain.ReplacementActivatingSharedWriter, domain.ReplacementNotifying, domain.ReplacementCompleted:
		if op.SuccessorSessionID == "" || op.SuccessorController == nil {
			return invalid
		}
	}
	switch op.Phase {
	case domain.ReplacementHandoffSettled, domain.ReplacementVerifyingResume, domain.ReplacementActivatingSharedWriter, domain.ReplacementNotifying, domain.ReplacementCompleted:
		if op.Bootstrap == nil || op.Bootstrap.SettledAt == nil {
			return invalid
		}
	}
	switch op.Phase {
	case domain.ReplacementActivatingSharedWriter, domain.ReplacementNotifying, domain.ReplacementCompleted:
		if op.ResumeEvidence == nil || op.ResumeEvidence.VerifiedAt == nil || op.ResumeEvidence.Probe == nil || op.ResumeEvidence.Probe.SettledAt == nil {
			return invalid
		}
	}
	if op.Phase == domain.ReplacementNotifying || op.Phase == domain.ReplacementCompleted {
		if op.WriterActivation == nil {
			return invalid
		}
	}
	if op.Phase == domain.ReplacementCompleted && (op.IntakeReleasedAt == nil || op.IntakeDrainRequestedAt == nil || len(op.Notifications) != 0 && (op.NotificationCompletion == nil || !op.NotificationCompletion.ValidFor(op))) {
		return invalid
	}
	if op.SourceStopIntentAt != nil || op.SourceStop != nil || op.SourceRetiredAt != nil {
		switch op.Phase {
		case domain.ReplacementCancelled, domain.ReplacementAwaitingSource, domain.ReplacementAwaitingHandoff, domain.ReplacementAwaitingTranscript:
			return invalid
		}
	}
	return nil
}

func validateReplacementTransition(ctx context.Context, q *gen.Queries, before, after domain.OrchestratorReplacement) error {
	if before.Phase == after.Phase {
		return nil
	}
	if ctx.Value(replacementRetainedRetryKey{}) == q && before.Phase == domain.ReplacementRecoveryRequired && after.Phase == before.ResumePhase {
		return nil
	}
	if before.Phase.Terminal() {
		return domain.ErrReplacementStateInvalid
	}
	if ctx.Value(replacementResetKey{}) == q && (after.Phase == domain.ReplacementPreflight || after.Phase == domain.ReplacementCancelled) {
		return nil // preserveReplacementAttempts separately verifies the archive and no-stop boundary.
	}
	if after.Phase == domain.ReplacementCancelled && before.FrozenAt == nil && before.SourceStopIntentAt == nil && before.SourceStop == nil && before.SourceRetiredAt == nil {
		return nil
	}
	if after.Phase == domain.ReplacementRecoveryRequired {
		return nil
	}
	if before.SourceStopIntentAt == nil && before.SourceStop == nil && before.SourceRetiredAt == nil {
		switch after.Phase {
		case domain.ReplacementAwaitingSource, domain.ReplacementAwaitingHandoff, domain.ReplacementAwaitingTranscript:
			return nil
		}
	}
	if ctx.Value(replacementUnattemptedStopKey{}) == q && before.Phase == domain.ReplacementExportingTranscript && after.Phase == domain.ReplacementAwaitingSource {
		return nil
	}
	next := map[domain.ReplacementPhase]domain.ReplacementPhase{
		domain.ReplacementAccepted:               domain.ReplacementPreflight,
		domain.ReplacementPreflight:              domain.ReplacementWaitingSourceIdle,
		domain.ReplacementWaitingSourceIdle:      domain.ReplacementAdoptingSharedFiles,
		domain.ReplacementAdoptingSharedFiles:    domain.ReplacementPreparingHandoff,
		domain.ReplacementPreparingHandoff:       domain.ReplacementHandoffVerified,
		domain.ReplacementHandoffVerified:        domain.ReplacementFreezingSharedWrites,
		domain.ReplacementFreezingSharedWrites:   domain.ReplacementExportingTranscript,
		domain.ReplacementExportingTranscript:    domain.ReplacementTranscriptVerified,
		domain.ReplacementTranscriptVerified:     domain.ReplacementRetiring,
		domain.ReplacementRetiring:               domain.ReplacementStarting,
		domain.ReplacementStarting:               domain.ReplacementControllerReady,
		domain.ReplacementControllerReady:        domain.ReplacementHandoffSettled,
		domain.ReplacementHandoffSettled:         domain.ReplacementVerifyingResume,
		domain.ReplacementVerifyingResume:        domain.ReplacementActivatingSharedWriter,
		domain.ReplacementActivatingSharedWriter: domain.ReplacementNotifying,
		domain.ReplacementNotifying:              domain.ReplacementCompleted,
	}
	if next[before.Phase] != after.Phase {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}
