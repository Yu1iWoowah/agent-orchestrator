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
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

type replacementNoticeCompletionKey struct{}

// Each target owns its bounded retry budget; one guarded worker cannot spend
// another worker's allowance. Positive outcomes always have separate capacity.
const replacementNoticeRetryLimit = 68

func noticePayloadHash(op domain.OrchestratorReplacement) string {
	sum := sha256.Sum256([]byte(op.WorkerFYI()))
	return hex.EncodeToString(sum[:])
}

func noticeTarget(op domain.OrchestratorReplacement, id domain.SessionID) (domain.ReplacementWorkerNotification, bool) {
	for _, target := range op.Notifications {
		if target.SessionID == id {
			return target, true
		}
	}
	return domain.ReplacementWorkerNotification{}, false
}

func decodeStoredNotice(ctx context.Context, q *gen.Queries, row gen.OrchestratorReplacementNotice) (domain.ReplacementNoticeReceipt, error) {
	var receipt domain.ReplacementNoticeReceipt
	decoder := json.NewDecoder(strings.NewReader(row.StateJson))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, err
	}
	if !validNoticeReceipt(receipt) || row.OperationID != receipt.OperationID || row.SessionID != string(receipt.SessionID) || row.ContactID != receipt.ContactID || row.State != string(receipt.State) {
		return receipt, domain.ErrReplacementStateInvalid
	}
	history, err := q.GetLatestReplacementWorkerNoticeHistory(ctx, gen.GetLatestReplacementWorkerNoticeHistoryParams{OperationID: receipt.OperationID, SessionID: string(receipt.SessionID)})
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, domain.ErrReplacementStateInvalid
	}
	if err != nil {
		return receipt, err
	}
	if history.StateJson != row.StateJson || history.Sequence != row.Revision || history.RecordedAt != row.UpdatedAt {
		return receipt, domain.ErrReplacementStateInvalid
	}
	receipt.Revision = row.Revision
	return receipt, nil
}

func decodeNotice(ctx context.Context, q *gen.Queries, op domain.OrchestratorReplacement, row gen.OrchestratorReplacementNotice) (domain.ReplacementNoticeReceipt, error) {
	receipt, err := decodeStoredNotice(ctx, q, row)
	if err != nil {
		return receipt, err
	}
	target, found := noticeTarget(op, receipt.SessionID)
	if !found || receipt.OperationID != op.ID || row.OperationID != op.ID || row.SessionID != string(receipt.SessionID) || receipt.ContactID != target.ContactID || row.ContactID != receipt.ContactID || row.State != string(receipt.State) || receipt.PayloadHash != noticePayloadHash(op) || !receipt.CreatedAt.Equal(target.CreatedAt) {
		return receipt, domain.ErrReplacementStateInvalid
	}
	return receipt, nil
}

func saveNotice(ctx context.Context, q *gen.Queries, receipt domain.ReplacementNoticeReceipt, at time.Time) (domain.ReplacementNoticeReceipt, error) {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return receipt, err
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	if receipt.Revision == 0 {
		err = q.InsertReplacementWorkerNotice(ctx, gen.InsertReplacementWorkerNoticeParams{OperationID: receipt.OperationID, SessionID: string(receipt.SessionID), ContactID: receipt.ContactID, State: string(receipt.State), StateJson: string(raw), UpdatedAt: stamp})
	} else {
		var changed int64
		changed, err = q.UpdateReplacementWorkerNotice(ctx, gen.UpdateReplacementWorkerNoticeParams{OperationID: receipt.OperationID, SessionID: string(receipt.SessionID), Revision: receipt.Revision, State: string(receipt.State), StateJson: string(raw), UpdatedAt: stamp})
		if err == nil && changed != 1 {
			err = domain.ErrReplacementStateInvalid
		}
	}
	if err != nil {
		return receipt, err
	}
	if err := q.AppendReplacementWorkerNoticeHistory(ctx, gen.AppendReplacementWorkerNoticeHistoryParams{OperationID: receipt.OperationID, SessionID: string(receipt.SessionID), StateJson: string(raw), RecordedAt: stamp}); err != nil {
		return receipt, err
	}
	receipt.Revision++
	return receipt, nil
}

func replacementNoticeSummary(ctx context.Context, q *gen.Queries, op domain.OrchestratorReplacement, at time.Time) (*domain.ReplacementNotificationCompletion, error) {
	if len(op.Notifications) == 0 {
		return nil, nil
	}
	rows, err := q.ListReplacementWorkerNotices(ctx, op.ID)
	if err != nil {
		return nil, err
	}
	if len(rows) != len(op.Notifications) {
		return nil, nil
	}
	summary := &domain.ReplacementNotificationCompletion{TargetCount: len(rows), CompletedAt: at.UTC()}
	receipts := make([]domain.ReplacementNoticeReceipt, 0, len(rows))
	for _, row := range rows {
		receipt, err := decodeNotice(ctx, q, op, row)
		if err != nil {
			return nil, err
		}
		switch receipt.State {
		case domain.ReplacementNoticeAccepted:
			summary.AcceptedCount++
		case domain.ReplacementNoticeSkipped:
			summary.SkippedCount++
		default:
			return nil, nil
		}
		if receipt.SettledAt == nil || receipt.SettledAt.IsZero() {
			return nil, domain.ErrReplacementStateInvalid
		}
		receipts = append(receipts, receipt)
	}
	raw, err := json.Marshal(receipts)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	summary.ReceiptHash = hex.EncodeToString(sum[:])
	return summary, nil
}

// ReconcileReplacementNotificationReceipts records only positive termination or
// existing delivery facts, then seals completion. It performs no provider I/O.
func (s *Store) ReconcileReplacementNotificationReceipts(ctx context.Context, id string, revision int64, at time.Time) (domain.OrchestratorReplacement, bool, error) {
	var result domain.OrchestratorReplacement
	applied := false
	if at.IsZero() {
		return result, false, domain.ErrReplacementStateInvalid
	}
	err := s.replacementTx(ctx, func(tx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(tx, id)
		if err != nil {
			return err
		}
		op, err := s.decodeCheckpointedReplacement(tx, q, row)
		if err != nil {
			return err
		}
		result = op
		if op.Phase == domain.ReplacementCompleted {
			applied = true
			return nil
		}
		if op.Revision != revision || op.Phase != domain.ReplacementNotifying || op.WriterActivation == nil || op.IntakeReleasedAt == nil || op.IntakeDrainRequestedAt == nil {
			return nil
		}
		if _, _, err := verifiedReplacementIntakeWriter(tx, q, op); err != nil {
			return err
		}
		for _, target := range op.Notifications {
			var receipt domain.ReplacementNoticeReceipt
			existing, err := q.GetReplacementWorkerNotice(tx, gen.GetReplacementWorkerNoticeParams{OperationID: id, SessionID: string(target.SessionID)})
			if err == nil {
				receipt, err = decodeNotice(tx, q, op, existing)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if receipt.State == domain.ReplacementNoticeAccepted || receipt.State == domain.ReplacementNoticeSkipped {
				continue
			}
			workerRow, err := q.GetSession(tx, target.SessionID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			} // Missing is not proof of termination.
			if err != nil {
				return err
			}
			worker := rowToRecord(workerRow)
			if worker.ProjectID != op.ProjectID || worker.Kind != domain.KindWorker {
				return domain.ErrReplacementStateInvalid
			}
			messages, err := q.SelectReplacementWorkerNoticeMessages(tx, gen.SelectReplacementWorkerNoticeMessagesParams{SessionID: &target.SessionID, ProjectID: &op.ProjectID, ContactID: target.ContactID, ExpectedText: op.WorkerFYI()})
			if err != nil {
				return err
			}
			if len(messages) > 1 {
				return domain.ErrReplacementStateInvalid
			}
			now := at.UTC()
			if receipt.Revision == 0 {
				receipt = domain.ReplacementNoticeReceipt{OperationID: id, SessionID: target.SessionID, ContactID: target.ContactID, PayloadHash: noticePayloadHash(op), CreatedAt: target.CreatedAt, Owner: worker.ControllerOwner(), Mode: domain.NormalizeSessionMode(worker.Mode)}
			}
			switch {
			case len(messages) == 1:
				receipt.ConversationID = messages[0].ConversationID
				receipt.State, receipt.Mode, receipt.MessageID, receipt.TurnID, receipt.EvidenceKind, receipt.ErrorCode, receipt.SettledAt = domain.ReplacementNoticeAccepted, domain.SessionModeChat, messages[0].ID, messages[0].TurnID.String, "chat_queue", "", &now
			case receipt.Mode == domain.SessionModeTUI && receipt.AttemptID != "" && receipt.Owner == worker.ControllerOwner() && worker.Metadata.RuntimeLaunchID != "" && worker.Metadata.AgentSessionID != "" && worker.Metadata.ConversationCheckpointState == domain.ConversationCheckpointCoordination && worker.Metadata.ConversationCheckpointGeneration == receipt.Owner.RuntimeLaunchID && worker.Metadata.ConversationCheckpointNativeID == receipt.Owner.AgentSessionID && worker.Metadata.ConversationCheckpointTurnID == receipt.NativeDeliveryID():
				receipt.State, receipt.TurnID, receipt.EvidenceKind, receipt.ErrorCode, receipt.SettledAt = domain.ReplacementNoticeAccepted, receipt.NativeDeliveryID(), "tui_checkpoint", "", &now
			case worker.IsTerminated:
				receipt.State, receipt.Owner, receipt.Mode, receipt.EvidenceKind, receipt.ErrorCode, receipt.SettledAt = domain.ReplacementNoticeSkipped, worker.ControllerOwner(), domain.NormalizeSessionMode(worker.Mode), "terminated", "TARGET_TERMINATED", &now
			default:
				continue
			}
			if _, err := saveNotice(tx, q, receipt, at); err != nil {
				return err
			}
		}
		summary, err := replacementNoticeSummary(tx, q, op, at)
		if err != nil {
			return err
		}
		if summary == nil {
			return nil
		}
		op.NotificationCompletion = summary
		op.Phase, op.ResumePhase, op.Error, op.UpdatedAt = domain.ReplacementCompleted, "", nil, at.UTC()
		saved, err := s.SaveOrchestratorReplacement(context.WithValue(tx, replacementNoticeCompletionKey{}, q), op)
		if err != nil {
			return err
		}
		if !saved {
			return domain.ErrReplacementStateInvalid
		}
		op.Revision++
		result = op
		applied = true
		return nil
	})
	return result, applied && err == nil, err
}

func preserveReplacementNotificationCompletion(ctx context.Context, q *gen.Queries, before, after domain.OrchestratorReplacement) error {
	if before.NotificationCompletion != nil {
		if !reflect.DeepEqual(before.NotificationCompletion, after.NotificationCompletion) {
			return domain.ErrReplacementStateInvalid
		}
		return nil
	}
	if after.NotificationCompletion == nil {
		return nil
	}
	if ctx.Value(replacementNoticeCompletionKey{}) != q {
		return domain.ErrReplacementStateInvalid
	}
	expected, err := replacementNoticeSummary(ctx, q, before, after.NotificationCompletion.CompletedAt)
	if err != nil {
		return err
	}
	if expected == nil || !reflect.DeepEqual(expected, after.NotificationCompletion) {
		return domain.ErrReplacementStateInvalid
	}
	return nil
}

// ReplacementWorkerNoticeReceipts is a read-only projection, including pending
// activation targets which have not yet acquired a delivery attempt.
func (s *Store) ReplacementWorkerNoticeReceipts(ctx context.Context, id string) ([]domain.ReplacementNoticeReceipt, error) {
	result := []domain.ReplacementNoticeReceipt{}
	err := s.replacementRead(ctx, func(tx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(tx, id)
		if err != nil {
			return err
		}
		op, err := s.decodeCheckpointedReplacement(tx, q, row)
		if err != nil {
			return err
		}
		for _, target := range op.Notifications {
			receipt := domain.ReplacementNoticeReceipt{OperationID: id, SessionID: target.SessionID, ContactID: target.ContactID, State: domain.ReplacementNoticePending, CreatedAt: target.CreatedAt, PayloadHash: noticePayloadHash(op)}
			row, err := q.GetReplacementWorkerNotice(tx, gen.GetReplacementWorkerNoticeParams{OperationID: id, SessionID: string(target.SessionID)})
			if err == nil {
				receipt, err = decodeNotice(tx, q, op, row)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			retries, err := q.CountReplacementWorkerNoticeRetries(tx, gen.CountReplacementWorkerNoticeRetriesParams{OperationID: id, SessionID: string(target.SessionID)})
			if err != nil {
				return err
			}
			if receipt.State == domain.ReplacementNoticePending && retries >= replacementNoticeRetryLimit {
				receipt.ErrorCode = "DELIVERY_RETRY_CAPACITY"
			}
			result = append(result, receipt)
		}
		return nil
	})
	return result, err
}

// Closed receipt shapes keep unknown/future evidence from becoming completion.
func validNoticeReceipt(p domain.ReplacementNoticeReceipt) bool {
	settled := p.SettledAt != nil && !p.SettledAt.IsZero()
	switch p.State {
	case domain.ReplacementNoticePending, domain.ReplacementNoticeUncertain:
		if settled || p.EvidenceKind != "" || p.MessageID != "" || p.TurnID != "" {
			return false
		}
		if p.State == domain.ReplacementNoticePending {
			return p.AttemptID == "" && p.AttemptedAt == nil && p.OutcomeRecordedAt == nil || p.AttemptID != "" && p.AttemptedAt != nil && p.OutcomeRecordedAt != nil
		}
		return p.AttemptID != "" && p.AttemptedAt != nil && !p.AttemptedAt.IsZero()
	case domain.ReplacementNoticeAccepted:
		if !settled || p.ErrorCode != "" {
			return false
		}
		switch p.EvidenceKind {
		case "chat_queue":
			return p.Mode == domain.SessionModeChat && p.ConversationID != "" && p.MessageID != "" && p.TurnID != ""
		case "tui_checkpoint":
			return p.Mode == domain.SessionModeTUI && p.AttemptID != "" && p.TurnID == p.NativeDeliveryID() && p.Owner.RuntimeLaunchID != "" && p.Owner.AgentSessionID != "" && p.Owner.AgentSessionIDLaunchID == p.Owner.RuntimeLaunchID
		}
	case domain.ReplacementNoticeSkipped:
		return settled && p.EvidenceKind == "terminated" && p.ErrorCode == "TARGET_TERMINATED" && p.Owner.IsTerminated
	}
	return false
}

// Activation and seed rollback share the writer transaction. A seed either
// disappears before activation captures targets, or retains its identity for
// rollback's positive MarkTerminated fallback. Never infer death from absence.
func (s *Store) replacementNotificationRetainsSeed(ctx context.Context, q *gen.Queries, id domain.SessionID) (bool, error) {
	row, err := q.GetSession(ctx, id)
	if err != nil {
		return false, err
	}
	worker := rowToRecord(row)
	if worker.Kind != domain.KindWorker || worker.ProjectID == "" {
		return false, nil
	}
	active, err := q.GetActiveOrchestratorReplacement(ctx, string(worker.ProjectID))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	op, err := s.decodeCheckpointedReplacement(ctx, q, active)
	if err != nil {
		// An unverified operation may retain this seed. Preserve its row so
		// rollback can record positive termination without erasing op facts.
		return true, nil
	}
	_, retained := noticeTarget(op, id)
	return retained, nil
}
