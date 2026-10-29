package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// Fact validation is shared by generic CAS. It does not produce lifecycle receipts.
func replacementActivationMatches(ctx context.Context, q *gen.Queries, op domain.OrchestratorReplacement, expected domain.SessionRecord) (bool, error) {
	proof, ok := op.ExpectedSuccessorController()
	p := op.ResumeEvidence
	if !ok || op.Phase != domain.ReplacementActivatingSharedWriter || expected.ID != op.SuccessorSessionID || p == nil || p.Probe == nil || p.VerifiedAt == nil || p.Probe.SettledAt == nil || p.Probe.TurnID == "" || expected.ControllerOwner() != proof.Owner || expected.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier || expected.Metadata.Model != op.TargetAgentConfig.Model || expected.Metadata.Effort != op.TargetAgentConfig.Effort || expected.Metadata.Permissions != op.TargetAgentConfig.Permissions {
		return false, nil
	}
	stateRow, err := q.GetProjectDocumentState(ctx, string(op.ProjectID))
	if err != nil {
		return false, err
	}
	var state domain.ProjectDocumentState
	if err := json.Unmarshal([]byte(stateRow.StateJson), &state); err != nil {
		return false, err
	}
	lease := state.Lease
	if lease == nil || !lease.Frozen || lease.SessionID != op.SourceSessionID || lease.Owner != op.SourceOwner || lease.ReplacementID != op.ID || op.Preparation == nil || lease.Epoch != op.Preparation.LeaseEpoch || lease.TokenHash == "" || state.SharedDir != op.SharedDir || op.Transcript == nil || op.Transcript.Receipt == nil || state.Evidence[op.ID+"/"+op.Transcript.AttemptID] != *op.Transcript.Receipt {
		return false, nil
	}
	row, err := q.GetSession(ctx, expected.ID)
	if err != nil {
		return false, err
	}
	current := rowToRecord(row)
	if current.Revision != expected.Revision || current.ProvisionState != domain.SessionProvisionReady || current.HibernatedAt != nil || current.ClientRequestID != op.SpawnRequestID || current.ClientRequestHash != op.SpawnRequestHash {
		return false, nil
	}
	source, err := q.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		return false, err
	}
	retired := op.SourceOwner
	retired.IsTerminated = true
	old := rowToRecord(source)
	if old.ProjectID != op.ProjectID || old.Kind != domain.KindOrchestrator || old.ControllerOwner() != retired || old.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
		return false, nil
	}
	conversation, err := q.SelectConversationBySession(ctx, &expected.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	message, err := q.SelectConversationMessageByClientID(ctx, gen.SelectConversationMessageByClientIDParams{ConversationID: conversation.ID, ClientMessageID: op.ResumeProbeClientID})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	m := messageToDomain(message)
	sum := sha256.Sum256([]byte(m.Text))
	if m.TurnID != p.Probe.TurnID || m.Role != domain.MessageRoleUser || m.Origin != domain.MessageOriginAutomation || m.AuthoredByUser || m.SenderSessionID != "" || m.DeliveryContentJSON != "" || m.Continuation || hex.EncodeToString(sum[:]) != p.Probe.PromptHash {
		return false, nil
	}
	turnRow, err := q.SelectConversationTurnByID(ctx, p.Probe.TurnID)
	if err != nil {
		return false, err
	}
	turn := turnToDomain(turnRow)
	if turn.ConversationID != conversation.ID || turn.HandledBySessionID != expected.ID || turnRow.ControllerGeneration != proof.Owner.ControllerGeneration || turn.State != domain.TurnStateCompleted || turn.ProviderTurnID == "" || turn.RolledBackAt != nil || turn.BranchID != conversation.ActiveBranchID {
		return false, nil
	}
	branch, err := q.SelectConversationBranch(ctx, gen.SelectConversationBranchParams{ConversationID: conversation.ID, BranchID: conversation.ActiveBranchID})
	if err != nil {
		return false, err
	}
	b := conversationBranchToDomain(branch)
	if b.SessionID != expected.ID || b.ProviderConversationID != p.AfterNativeID {
		return false, nil
	}
	rows, err := q.SelectConversationNativeThreadEvidence(ctx, gen.SelectConversationNativeThreadEvidenceParams{ConversationID: conversation.ID, SessionID: expected.ID})
	if err != nil {
		return false, err
	}
	evidence := make([]domain.ConversationNativeThreadEvidence, 0, len(rows))
	for _, entry := range rows {
		evidence = append(evidence, domain.ConversationNativeThreadEvidence{BranchID: entry.BranchID, ProviderTurnID: entry.ProviderTurnID, ProviderConversationID: entry.ProviderConversationID})
	}
	if domain.IndexNativeThreadEvidence(evidence).Classify(turn.BranchID, turn.ProviderTurnID, p.AfterNativeID) != domain.NativeThreadMain {
		return false, nil
	}
	ack, ok := domain.ReplacementProbeAcknowledgment(op)
	if !ok {
		return false, nil
	}
	replies, err := q.SelectReplacementProbeReplies(ctx, gen.SelectReplacementProbeRepliesParams{ConversationID: conversation.ID, TurnID: sql.NullString{String: turn.ID, Valid: true}})
	if err != nil {
		return false, err
	}
	if len(replies) > domain.MaxReplacementProbeReplyMessages {
		return false, nil
	}
	for _, reply := range replies {
		if reply.ByteLength <= domain.MaxReplacementProbeReplyBytes && strings.Contains(reply.BoundedText, ack) {
			return true, nil
		}
	}
	return false, nil
}

func validateReplacementBootstrap(op domain.OrchestratorReplacement) error {
	b := op.Bootstrap
	if b == nil {
		return nil
	}
	_, err := hex.DecodeString(b.PromptHash)
	if err != nil || len(b.PromptHash) != 64 || b.IntentAt.IsZero() || op.BootstrapClientID == "" || op.SuccessorController == nil || op.SourceRetiredAt == nil || b.SettledAt != nil && (b.TurnID == "" || b.SettledAt.IsZero()) {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}

func preserveReplacementBootstrap(before, after domain.OrchestratorReplacement) error {
	a, b := before.Bootstrap, after.Bootstrap
	if a == nil {
		if b != nil && after.Phase != domain.ReplacementControllerReady {
			return domain.ErrReplacementStateInvalid
		}
		return nil
	}
	if b == nil || a.PromptHash != b.PromptHash || !a.IntentAt.Equal(b.IntentAt) || a.TurnID != "" && a.TurnID != b.TurnID || a.SettledAt != nil && (b.SettledAt == nil || !a.SettledAt.Equal(*b.SettledAt)) {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}

func validateReplacementSuccessorController(op domain.OrchestratorReplacement) error {
	p := op.SuccessorController
	if p == nil {
		return nil
	}
	owner := p.Owner
	if op.SourceRetiredAt == nil || op.SuccessorSessionID == "" || op.Transcript == nil || owner.IsTerminated || owner.Harness != domain.HarnessClaudeCode || owner.Mode != domain.SessionModeChat || owner.ControllerGeneration == "" || owner.ProviderConversationID == "" || p.CapabilityVerifier == "" || p.CommittedAt.IsZero() {
		return domain.ErrReplacementStateInvalid
	}
	for _, cut := range op.Transcript.NativeCutoffs {
		if cut.NativeID == owner.ProviderConversationID {
			return domain.ErrReplacementStateInvalid
		}
	}
	return nil
}

func preserveReplacementSuccessorController(before, after domain.OrchestratorReplacement) error {
	prior, next := before.SuccessorController, after.SuccessorController
	if prior == nil {
		if next != nil {
			return domain.ErrReplacementStateInvalid
		}
		return nil
	}
	if next == nil || prior.Owner != next.Owner || prior.CapabilityVerifier != next.CapabilityVerifier || !prior.CommittedAt.Equal(next.CommittedAt) {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}

func validateReplacementResume(op domain.OrchestratorReplacement) error {
	p := op.ResumeEvidence
	if p == nil || p.StartedAt.IsZero() {
		return nil
	} // Earlier partial observations are not stop authority.
	initial := op.SuccessorController
	if initial == nil || op.SourceRetiredAt == nil || op.Bootstrap == nil || op.Bootstrap.SettledAt == nil || p.BeforeOwner != initial.Owner || p.BeforeCapabilityVerifier != initial.CapabilityVerifier || p.BeforeNativeID != initial.Owner.ProviderConversationID {
		return domain.ErrReplacementStateInvalid
	}
	stop := p.StopReceipt
	if p.StopConfirmed != (stop != nil) {
		return domain.ErrReplacementStateInvalid
	}
	if stop != nil && (stop.Owner != p.BeforeOwner || stop.CapabilityVerifier != p.BeforeCapabilityVerifier || stop.NativeID != p.BeforeNativeID || stop.Method != "chat-hibernate" || stop.ConfirmedAt.IsZero()) {
		return domain.ErrReplacementStateInvalid
	}
	if p.ResumeIntentAt != nil || p.ResumeGeneration != "" {
		if !p.StopConfirmed || p.ResumeIntentAt == nil || p.ResumeIntentAt.IsZero() || p.ResumeGeneration == "" || p.ResumeGeneration == p.BeforeOwner.ControllerGeneration {
			return domain.ErrReplacementStateInvalid
		}
	}
	if p.ResumedAt != nil {
		if p.ResumedAt.IsZero() || p.ResumeIntentAt == nil || !p.HistoryVerified || p.AfterNativeID != p.BeforeNativeID || p.AfterOwner.ProviderConversationID != p.BeforeNativeID || p.AfterOwner.ControllerGeneration != p.ResumeGeneration || p.AfterOwner.IsTerminated || p.AfterOwner.Harness != domain.HarnessClaudeCode || p.AfterOwner.Mode != domain.SessionModeChat || p.AfterCapabilityVerifier == "" || p.AfterCapabilityVerifier == p.BeforeCapabilityVerifier {
			return domain.ErrReplacementStateInvalid
		}
	} else if p.AfterNativeID != "" || p.AfterOwner != (domain.SessionControllerOwner{}) || p.AfterCapabilityVerifier != "" || p.HistoryVerified {
		return domain.ErrReplacementStateInvalid
	}
	if probe := p.Probe; probe != nil {
		_, err := hex.DecodeString(probe.PromptHash)
		if p.ResumedAt == nil || op.ResumeProbeClientID == "" || !replacementUUID(probe.Nonce) || probe.IntentAt.IsZero() || len(probe.PromptHash) != 64 || err != nil || probe.SettledAt != nil && (probe.TurnID == "" || probe.SettledAt.IsZero()) {
			return domain.ErrReplacementStateInvalid
		}
	}
	if p.VerifiedAt != nil && (p.Probe == nil || p.Probe.SettledAt == nil || p.VerifiedAt.IsZero() || !p.VerifiedAt.Equal(*p.Probe.SettledAt)) {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}

func preserveReplacementResume(before, after domain.OrchestratorReplacement) error {
	a, b := before.ResumeEvidence, after.ResumeEvidence
	if a == nil || a.StartedAt.IsZero() {
		if b != nil && !b.StartedAt.IsZero() && after.Phase != domain.ReplacementVerifyingResume {
			return domain.ErrReplacementStateInvalid
		}
		return nil
	}
	if b == nil || !a.StartedAt.Equal(b.StartedAt) || a.BeforeOwner != b.BeforeOwner || a.BeforeNativeID != b.BeforeNativeID || a.BeforeCapabilityVerifier != b.BeforeCapabilityVerifier || a.StopConfirmed && !b.StopConfirmed {
		return domain.ErrReplacementStateInvalid
	}
	if a.StopReceipt != nil {
		x, y := a.StopReceipt, b.StopReceipt
		if y == nil || x.Owner != y.Owner || x.CapabilityVerifier != y.CapabilityVerifier || x.NativeID != y.NativeID || x.Method != y.Method || !x.ConfirmedAt.Equal(y.ConfirmedAt) {
			return domain.ErrReplacementStateInvalid
		}
	}
	if a.ResumeIntentAt != nil && (!replacementTimesEqual(a.ResumeIntentAt, b.ResumeIntentAt) || a.ResumeGeneration != b.ResumeGeneration) {
		return domain.ErrReplacementStateInvalid
	}
	if a.ResumedAt != nil && (!replacementTimesEqual(a.ResumedAt, b.ResumedAt) || a.AfterOwner != b.AfterOwner || a.AfterNativeID != b.AfterNativeID || a.AfterCapabilityVerifier != b.AfterCapabilityVerifier || !b.HistoryVerified) {
		return domain.ErrReplacementStateInvalid
	}
	if a.Probe == nil {
		if b.Probe != nil && after.Phase != domain.ReplacementVerifyingResume {
			return domain.ErrReplacementStateInvalid
		}
	} else {
		x, y := a.Probe, b.Probe
		if y == nil || x.Nonce != y.Nonce || x.PromptHash != y.PromptHash || !x.IntentAt.Equal(y.IntentAt) || x.TurnID != "" && x.TurnID != y.TurnID || x.SettledAt != nil && !replacementTimesEqual(x.SettledAt, y.SettledAt) {
			return domain.ErrReplacementStateInvalid
		}
	}
	if a.VerifiedAt != nil && !replacementTimesEqual(a.VerifiedAt, b.VerifiedAt) {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}

type replacementSourceReleaseKey struct{}

func preserveReplacementSourceRelease(ctx context.Context, q *gen.Queries, before, after domain.OrchestratorReplacement) error {
	if before.SourceIntakeReleasedAt != nil {
		if ctx.Value(replacementResetKey{}) != q && !replacementTimesEqual(before.SourceIntakeReleasedAt, after.SourceIntakeReleasedAt) {
			return domain.ErrReplacementStateInvalid
		}
	} else if after.SourceIntakeReleasedAt != nil && ctx.Value(replacementSourceReleaseKey{}) != q {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}

type replacementSpawnAttemptKey struct{}

func validateReplacementSpawnAttempts(op domain.OrchestratorReplacement) error {
	if op.SpawnRetryGeneration < 0 || (op.SpawnRetryGeneration != 0 && len(op.SpawnAttempts) == 0) {
		return domain.ErrReplacementStateInvalid
	}
	seen := map[string]bool{}
	for i, attempt := range op.SpawnAttempts {
		if attempt.ID == "" || seen[attempt.ID] || attempt.StartedAt.IsZero() || attempt.RetryGeneration < 0 || attempt.RetryGeneration > op.SpawnRetryGeneration || op.SourceRetiredAt == nil || op.SuccessorSessionID == "" {
			return domain.ErrReplacementStateInvalid
		}
		if attempt.LaunchIntentAt != nil && attempt.LaunchIntentAt.IsZero() || attempt.FailedAt != nil && attempt.FailedAt.IsZero() {
			return domain.ErrReplacementStateInvalid
		}
		if i > 0 {
			prior := op.SpawnAttempts[i-1]
			if prior.LaunchIntentAt != nil || prior.FailedAt == nil || prior.RetryGeneration >= attempt.RetryGeneration {
				return domain.ErrReplacementStateInvalid
			}
		}
		seen[attempt.ID] = true
	}
	return nil
}

func preserveReplacementSpawnAttempts(ctx context.Context, q *gen.Queries, before, after domain.OrchestratorReplacement) error {
	if !reflect.DeepEqual(before.SpawnAttempts, after.SpawnAttempts) && ctx.Value(replacementSpawnAttemptKey{}) != q {
		return domain.ErrReplacementStateInvalid
	}
	if before.SpawnRetryGeneration != after.SpawnRetryGeneration {
		last, exists := before.LatestSpawnAttempt()
		if !exists || last.LaunchIntentAt != nil || before.Phase != domain.ReplacementRecoveryRequired || before.ResumePhase != domain.ReplacementStarting || after.Phase != domain.ReplacementStarting || after.SpawnRetryGeneration != before.SpawnRetryGeneration+1 {
			return domain.ErrReplacementStateInvalid
		}
	}
	return nil
}
