package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"reflect"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// Runtime observations become receipts only when backed by the current native
// owner and canonical ledger in this same transaction. The exported coordinator
// adapter is not authority to invent successful provider actions.
func validateReplacementRuntimePublication(ctx context.Context, q *gen.Queries, before, after domain.OrchestratorReplacement) error {
	invalid := domain.ErrReplacementStateInvalid
	if before.SourceStop == nil && after.SourceStop != nil {
		row, err := q.GetSession(ctx, after.SourceSessionID)
		if err != nil {
			return err
		}
		source := rowToRecord(row)
		if source.ProjectID != after.ProjectID || source.Kind != domain.KindOrchestrator || !after.SourceStop.MatchesSource(source) {
			return invalid
		}
	}
	changedBootstrap := !reflect.DeepEqual(before.Bootstrap, after.Bootstrap)
	changedResume := !reflect.DeepEqual(before.ResumeEvidence, after.ResumeEvidence)
	if !changedBootstrap && !changedResume {
		return nil
	}
	row, err := q.GetSession(ctx, after.SuccessorSessionID)
	if err != nil {
		return err
	}
	target := rowToRecord(row)
	proof, ok := after.ExpectedSuccessorController()
	if after.ResumeEvidence != nil && after.ResumeEvidence.ResumedAt == nil {
		if after.SuccessorController == nil {
			return invalid
		}
		proof, ok = *after.SuccessorController, true
	}
	if !ok || target.ProjectID != after.ProjectID || target.Kind != domain.KindOrchestrator || target.IsTerminated || target.ClientRequestID != after.SpawnRequestID || target.ClientRequestHash != after.SpawnRequestHash || target.ControllerOwner() != proof.Owner || target.Metadata.BrowserCapabilityVerifier != proof.CapabilityVerifier {
		return invalid
	}
	if changedBootstrap && after.Bootstrap != nil && after.Bootstrap.TurnID != "" {
		b := after.Bootstrap
		if err := verifyReplacementControlLedger(ctx, q, after, after.BootstrapClientID, b.TurnID, b.PromptHash, proof.Owner, b.SettledAt != nil, ""); err != nil {
			return err
		}
	}
	if !changedResume || after.ResumeEvidence == nil {
		return nil
	}
	p := after.ResumeEvidence
	if p.StopConfirmed && (before.ResumeEvidence == nil || !before.ResumeEvidence.StopConfirmed) {
		if p.StopReceipt == nil || !p.StopReceipt.MatchesSource(target) {
			return invalid
		}
	}
	if p.ResumedAt != nil && (before.ResumeEvidence == nil || before.ResumeEvidence.ResumedAt == nil) {
		if target.HibernatedAt != nil || target.ProvisionState != domain.SessionProvisionReady {
			return invalid
		}
		c, err := q.SelectConversationBySession(ctx, &target.ID)
		if err != nil {
			return err
		}
		b, err := q.SelectConversationBranch(ctx, gen.SelectConversationBranchParams{ConversationID: c.ID, BranchID: c.ActiveBranchID})
		if err != nil {
			return err
		}
		if !b.SessionID.Valid || b.SessionID.String != string(target.ID) || b.ProviderConversationID != p.BeforeNativeID || p.AfterNativeID != b.ProviderConversationID {
			return invalid
		}
	}
	if p.Probe != nil && p.Probe.TurnID != "" {
		ack := ""
		if p.Probe.SettledAt != nil {
			var ok bool
			ack, ok = domain.ReplacementProbeAcknowledgment(after)
			if !ok {
				return invalid
			}
		}
		if err := verifyReplacementControlLedger(ctx, q, after, after.ResumeProbeClientID, p.Probe.TurnID, p.Probe.PromptHash, proof.Owner, p.Probe.SettledAt != nil, ack); err != nil {
			return err
		}
	}
	return nil
}

func verifyReplacementControlLedger(ctx context.Context, q *gen.Queries, op domain.OrchestratorReplacement, clientID, turnID, promptHash string, owner domain.SessionControllerOwner, settled bool, ack string) error {
	invalid := domain.ErrReplacementStateInvalid
	c, err := q.SelectConversationBySession(ctx, &op.SuccessorSessionID)
	if err != nil {
		return err
	}
	m, err := q.SelectConversationMessageByClientID(ctx, gen.SelectConversationMessageByClientIDParams{ConversationID: c.ID, ClientMessageID: clientID})
	if err != nil {
		return err
	}
	message := messageToDomain(m)
	if message.AuthoredByUser || message.Continuation || message.SenderProjectID != "" {
		return invalid
	}
	sum := sha256.Sum256([]byte(m.Text))
	if m.Role != domain.MessageRoleUser || m.Origin != domain.MessageOriginAutomation || m.SenderSessionID != "" || m.DeliveryContentJson != "" || !m.TurnID.Valid || m.TurnID.String != turnID || hex.EncodeToString(sum[:]) != promptHash {
		return invalid
	}
	row, err := q.SelectConversationTurnByID(ctx, turnID)
	if err != nil {
		return err
	}
	t := turnToDomain(row)
	if t.ConversationID != c.ID || t.HandledBySessionID != op.SuccessorSessionID || row.ControllerGeneration != owner.ControllerGeneration || t.RolledBackAt != nil {
		return invalid
	}
	if !settled {
		return nil
	}
	if t.State != domain.TurnStateCompleted || t.ProviderTurnID == "" || t.BranchID != c.ActiveBranchID {
		return invalid
	}
	branch, err := q.SelectConversationBranch(ctx, gen.SelectConversationBranchParams{ConversationID: c.ID, BranchID: t.BranchID})
	if err != nil {
		return err
	}
	if !branch.SessionID.Valid || branch.SessionID.String != string(op.SuccessorSessionID) || branch.ProviderConversationID != owner.ProviderConversationID {
		return invalid
	}
	rows, err := q.SelectConversationNativeThreadEvidence(ctx, gen.SelectConversationNativeThreadEvidenceParams{ConversationID: c.ID, SessionID: op.SuccessorSessionID})
	if err != nil {
		return err
	}
	evidence := make([]domain.ConversationNativeThreadEvidence, 0, len(rows))
	for _, r := range rows {
		evidence = append(evidence, domain.ConversationNativeThreadEvidence{BranchID: r.BranchID, ProviderTurnID: r.ProviderTurnID, ProviderConversationID: r.ProviderConversationID})
	}
	if domain.IndexNativeThreadEvidence(evidence).Classify(t.BranchID, t.ProviderTurnID, owner.ProviderConversationID) != domain.NativeThreadMain {
		return invalid
	}
	if ack != "" {
		replies, err := q.SelectReplacementProbeReplies(ctx, gen.SelectReplacementProbeRepliesParams{ConversationID: c.ID, TurnID: sql.NullString{String: turnID, Valid: true}})
		if err != nil {
			return err
		}
		if len(replies) > domain.MaxReplacementProbeReplyMessages {
			return invalid
		}
		for _, r := range replies {
			if r.ByteLength <= domain.MaxReplacementProbeReplyBytes && strings.Contains(r.BoundedText, ack) {
				return nil
			}
		}
		return invalid
	}
	return nil
}
