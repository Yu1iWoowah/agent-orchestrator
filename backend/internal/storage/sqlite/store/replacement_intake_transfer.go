package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func preserveReplacementIntakeTransfer(ctx context.Context, q *gen.Queries, before, after domain.OrchestratorReplacement) error {
	if before.IntakeTransfer != nil {
		if !reflect.DeepEqual(before.IntakeTransfer, after.IntakeTransfer) {
			return domain.ErrReplacementStateInvalid
		}
	} else if after.IntakeTransfer != nil && ctx.Value(replacementIntakeReleaseKey{}) != q {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}

// transferReplacementIntake runs only after the activated writer was checked in
// this same transaction. Completed source history and all payload bytes stay.
func transferReplacementIntake(ctx context.Context, q *gen.Queries, op domain.OrchestratorReplacement, target domain.SessionRecord, conversation gen.Conversation, at time.Time) (*domain.ReplacementIntakeTransfer, error) {
	rows, err := q.SelectReplacementPendingMessages(ctx, op.ID)
	if err != nil {
		return nil, err
	}
	return transferReplacementIntakeRows(ctx, q, op, target, conversation, at, rows)
}

func transferReplacementIntakeRows(ctx context.Context, q *gen.Queries, op domain.OrchestratorReplacement, target domain.SessionRecord, conversation gen.Conversation, at time.Time, rows []gen.SelectReplacementPendingMessagesRow) (*domain.ReplacementIntakeTransfer, error) {
	result := &domain.ReplacementIntakeTransfer{RecordedAt: at, SourceConversationIDs: []string{}}

	sources := map[string]bool{}
	turns := map[string]bool{}
	for _, m := range rows {
		if !m.TurnID.Valid || turns[m.TurnID.String] {
			return nil, domain.ErrReplacementStateInvalid
		}
		turns[m.TurnID.String] = true
		turn, err := q.SelectConversationTurnByID(ctx, m.TurnID.String)
		if err != nil {
			return nil, err
		}
		if turn.ConversationID != m.ConversationID || turn.State != domain.TurnStateQueued || turn.StartedAt.Valid || turn.CompletedAt.Valid || turn.ProviderTurnID != "" || turn.ProviderInputText != "" || turn.PromotionStartedAt.Valid || turn.PromotedToTurnID.Valid || turn.RolledBackAt.Valid || turn.HandledByReviewID.Valid || turn.BatchedInput != 0 {
			return nil, domain.ErrReplacementIntakeUncertain
		}
		messages, err := q.CountReplacementTurnMessages(ctx, m.TurnID)
		if err != nil {
			return nil, err
		}
		activities, err := q.CountReplacementTurnActivities(ctx, m.TurnID)
		if err != nil {
			return nil, err
		}
		if messages != 1 || activities != 0 {
			return nil, domain.ErrReplacementIntakeUncertain
		}
		accepted, err := q.GetSession(ctx, turn.HandledBySessionID)
		if err != nil {
			return nil, err
		}
		if accepted.ProjectID == nil || *accepted.ProjectID != op.ProjectID || accepted.Kind != domain.KindOrchestrator {
			return nil, domain.ErrReplacementIntakeConflict
		}
		if m.ClientMessageID != "" {
			existing, err := q.SelectConversationMessageByClientID(ctx, gen.SelectConversationMessageByClientIDParams{ConversationID: conversation.ID, ClientMessageID: m.ClientMessageID})
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if err == nil && existing.ID != m.ID {
				return nil, domain.ErrReplacementIntakeConflict
			}
		}
		hash, err := domain.ReplacementIntakePayloadHash(domain.QueuedTurn{TurnID: turn.ID, Text: m.Text, ClientMessageID: m.ClientMessageID, Origin: m.Origin, DeliveryContentJSON: m.DeliveryContentJson, SenderSessionID: m.SenderSessionID, SenderProjectID: m.SenderProjectID, SenderDisplayName: m.SenderDisplayName})
		if err != nil {
			return nil, err
		}
		sequence := m.Sequence
		if m.ConversationID != conversation.ID {
			sequence, err = q.NextConversationSequence(ctx, gen.NextConversationSequenceParams{UpdatedAt: at, ID: conversation.ID})
			if err != nil {
				return nil, err
			}
			result.MovedMessages++
		}
		if err := q.RecordReplacementIntakeTransfer(ctx, gen.RecordReplacementIntakeTransferParams{OperationID: op.ID, MessageID: m.ID, TurnID: turn.ID, SourceConversationID: m.ConversationID, SourceSequence: m.Sequence, SourceSessionID: string(turn.HandledBySessionID), SourceControllerGeneration: turn.ControllerGeneration, SourceBranchID: turn.BranchID, TargetConversationID: conversation.ID, TargetSequence: sequence, PayloadHash: hash, TransferredAt: at}); err != nil {
			return nil, err
		}
		moved, err := q.MoveReplacementQueuedTurn(ctx, gen.MoveReplacementQueuedTurnParams{TargetConversationID: conversation.ID, TargetBranchID: conversation.ActiveBranchID, TargetSessionID: target.ID, TargetGeneration: target.Metadata.ControllerGeneration, TargetSequence: sequence, TurnID: turn.ID, SourceConversationID: m.ConversationID})
		if err != nil {
			return nil, err
		}
		if moved != 1 {
			return nil, domain.ErrReplacementIntakeUncertain
		}
		moved, err = q.MoveReplacementQueuedMessage(ctx, gen.MoveReplacementQueuedMessageParams{TargetConversationID: conversation.ID, TargetBranchID: conversation.ActiveBranchID, TargetSequence: sequence, MessageID: m.ID, TurnID: m.TurnID, SourceConversationID: m.ConversationID})
		if err != nil {
			return nil, err
		}
		if moved != 1 {
			return nil, domain.ErrReplacementStateInvalid
		}
		result.RetargetedMessages++
		sources[m.ConversationID] = true
	}
	for source := range sources {
		result.SourceConversationIDs = append(result.SourceConversationIDs, source)
		if source != conversation.ID {
			if err := copyReplacementQueuedEditReceipts(ctx, q, source, conversation.ID); err != nil {
				return nil, err
			}
		}
	}
	slices.Sort(result.SourceConversationIDs)
	return result, nil
}

// Queued-edit receipts contain an opaque request hash, not a target turn ID.
// Copy their metadata instead of rewriting/removing the source's receipt set.
func copyReplacementQueuedEditReceipts(ctx context.Context, q *gen.Queries, source, target string) error {
	receipts, err := q.SelectReplacementQueuedEditReceipts(ctx, source)
	if err != nil {
		return err
	}
	for _, receipt := range receipts {
		prior, err := q.SelectConversationQueuedEditDelivery(ctx, gen.SelectConversationQueuedEditDeliveryParams{ConversationID: target, ClientMessageID: receipt.ClientMessageID})
		if err == nil {
			if prior != receipt.RequestHash {
				return domain.ErrReplacementIntakeConflict
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := q.InsertConversationQueuedEditDelivery(ctx, gen.InsertConversationQueuedEditDeliveryParams{ConversationID: target, ClientMessageID: receipt.ClientMessageID, RequestHash: receipt.RequestHash, CreatedAt: receipt.CreatedAt}); err != nil {
			return err
		}
	}
	return nil
}
