package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

var _ ports.ReplacementDormantWorkerNoticeQueue = (*Store)(nil)

// QueueDormantReplacementWorkerNotice performs no runtime I/O. Queue insertion
// and its acceptance receipt commit together; the worker wakes only naturally.
func (s *Store) QueueDormantReplacementWorkerNotice(ctx context.Context, id string, revision int64, targetID domain.SessionID, expected domain.SessionControllerOwner, at time.Time) (bool, error) {
	accepted := false
	if at.IsZero() {
		return false, domain.ErrReplacementStateInvalid
	}
	err := s.replacementTx(ctx, func(ctx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(ctx, id)
		if err != nil {
			return err
		}
		op, err := s.decodeCheckpointedReplacement(ctx, q, row)
		if err != nil {
			return err
		}
		target, found := noticeTarget(op, targetID)
		if !found {
			return domain.ErrReplacementStateInvalid
		}
		if op.Revision != revision || op.Phase != domain.ReplacementNotifying || op.IntakeReleasedAt == nil || op.IntakeDrainRequestedAt == nil {
			return nil
		}
		if _, _, err := verifiedReplacementIntakeWriter(ctx, q, op); err != nil {
			return err
		}
		workerRow, err := q.GetSession(ctx, targetID)
		if err != nil {
			return err
		}
		worker := rowToRecord(workerRow)
		if worker.Kind != domain.KindWorker || worker.ProjectID != op.ProjectID || worker.ControllerOwner() != expected {
			return domain.ErrReplacementSourceChanged
		}
		if worker.IsTerminated || worker.IsTaskPreparation || domain.NormalizeSessionMode(worker.Mode) != domain.SessionModeChat || worker.HibernatedAt == nil && worker.ProvisionState == domain.SessionProvisionReady {
			return nil
		}
		var receipt domain.ReplacementNoticeReceipt
		prior, err := q.GetReplacementWorkerNotice(ctx, gen.GetReplacementWorkerNoticeParams{OperationID: id, SessionID: string(targetID)})
		if err == nil {
			receipt, err = decodeNotice(ctx, q, op, prior)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if receipt.State == domain.ReplacementNoticeAccepted || receipt.State == domain.ReplacementNoticeSkipped {
			accepted = receipt.State == domain.ReplacementNoticeAccepted
			return nil
		}
		if receipt.State == domain.ReplacementNoticeUncertain {
			// Never create a fresh scope for an unconfirmed earlier write.
			if receipt.Mode != domain.SessionModeChat {
				return nil
			}
			current, err := q.SelectConversationBySession(ctx, &worker.ID)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if current.ID != receipt.ConversationID {
				return nil
			}
		}
		conversation, err := dormantNoticeConversation(ctx, q, worker, at)
		if err != nil {
			return err
		}
		message, err := q.SelectConversationMessageByClientID(ctx, gen.SelectConversationMessageByClientIDParams{ConversationID: conversation.ID, ClientMessageID: target.ContactID})
		if errors.Is(err, sql.ErrNoRows) {
			// An uncertain write may only reuse the very same Chat queue. A
			// prior TUI write or changed queue never authorizes blind replay.
			if receipt.State == domain.ReplacementNoticeUncertain && (receipt.Mode != domain.SessionModeChat || receipt.ConversationID != conversation.ID) {
				return nil
			}
			sequence, sequenceErr := q.NextConversationSequence(ctx, gen.NextConversationSequenceParams{ID: conversation.ID, UpdatedAt: at})
			if sequenceErr != nil {
				return sequenceErr
			}
			turnID, messageID := uuid.NewString(), uuid.NewString()
			if err := q.InsertConversationTurn(ctx, gen.InsertConversationTurnParams{ID: turnID, ConversationID: conversation.ID, HandledBySessionID: targetID, State: domain.TurnStateQueued, RequestedAt: at}); err != nil {
				return err
			}
			if err := q.InsertConversationMessage(ctx, gen.InsertConversationMessageParams{ID: messageID, ConversationID: conversation.ID, TurnID: nullableString(turnID), Sequence: sequence, Role: domain.MessageRoleUser, Origin: domain.MessageOriginAutomation, Text: op.WorkerFYI(), ClientMessageID: target.ContactID, CreatedAt: at, UpdatedAt: at}); err != nil {
				return err
			}
			message, err = q.SelectConversationMessageByClientID(ctx, gen.SelectConversationMessageByClientIDParams{ConversationID: conversation.ID, ClientMessageID: target.ContactID})
		}
		if err != nil {
			return err
		}
		if message.Role != domain.MessageRoleUser || message.Origin != domain.MessageOriginAutomation || message.Text != op.WorkerFYI() || message.DeliveryContentJson != "" || !message.TurnID.Valid {
			return domain.ErrClientMessageConflict
		}
		turn, err := q.SelectConversationTurnByID(ctx, message.TurnID.String)
		if err != nil {
			return err
		}
		if turn.ConversationID != conversation.ID || turn.HandledBySessionID != targetID {
			return domain.ErrReplacementSourceChanged
		}
		if receipt.Revision == 0 {
			receipt = domain.ReplacementNoticeReceipt{OperationID: id, SessionID: targetID, ContactID: target.ContactID, Owner: worker.ControllerOwner(), CreatedAt: target.CreatedAt, PayloadHash: noticePayloadHash(op)}
		}
		now := at.UTC()
		receipt.State, receipt.Mode, receipt.ConversationID, receipt.MessageID, receipt.TurnID = domain.ReplacementNoticeAccepted, domain.SessionModeChat, conversation.ID, message.ID, message.TurnID.String
		receipt.EvidenceKind, receipt.ErrorCode, receipt.SettledAt = "chat_queue", "", &now
		if _, err := saveNotice(ctx, q, receipt, at); err != nil {
			return err
		}
		accepted = true
		return nil
	})
	return accepted && err == nil, err
}

func dormantNoticeConversation(ctx context.Context, q *gen.Queries, worker domain.SessionRecord, at time.Time) (gen.Conversation, error) {
	conversation, err := q.SelectConversationBySession(ctx, &worker.ID)
	if errors.Is(err, sql.ErrNoRows) {
		id := uuid.NewString()
		rootID := id + ":root"
		if err := q.InsertConversation(ctx, gen.InsertConversationParams{ID: id, Scope: domain.ConversationScopeSession, ProjectID: &worker.ProjectID, SessionID: &worker.ID, CurrentSessionID: &worker.ID, ActiveBranchID: rootID, CreatedAt: at, UpdatedAt: at}); err != nil {
			return conversation, err
		}
		if err := q.InsertConversationBranch(ctx, gen.InsertConversationBranchParams{ID: rootID, ConversationID: id, SessionID: nullableString(string(worker.ID)), ProviderConversationID: worker.Metadata.ProviderConversationID, ProviderScopeID: rootID, ProviderIdsScoped: 1, CreatedAt: at}); err != nil {
			return conversation, err
		}
		conversation, err = q.SelectConversationByID(ctx, id)
	}
	if err != nil {
		return conversation, err
	}
	if conversation.Scope != domain.ConversationScopeSession || conversation.ProjectID == nil || *conversation.ProjectID != worker.ProjectID || conversation.SessionID == nil || *conversation.SessionID != worker.ID || conversation.CurrentSessionID == nil || *conversation.CurrentSessionID != worker.ID {
		return conversation, domain.ErrConversationRebound
	}
	return conversation, nil
}
