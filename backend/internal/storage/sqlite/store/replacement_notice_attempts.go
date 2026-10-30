package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// BeginReplacementWorkerNotice binds a pre-write intent to the exact worker.
// A repeated Chat attempt can reuse its canonical queue key; TUI cannot replay
// a potentially attempted write. This method never starts or wakes a worker.
func (s *Store) BeginReplacementWorkerNotice(ctx context.Context, id string, revision int64, targetID domain.SessionID, expected domain.SessionControllerOwner, at time.Time) (domain.ReplacementNoticeReceipt, bool, error) {
	var result domain.ReplacementNoticeReceipt
	send := false
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
		target, found := noticeTarget(op, targetID)
		if !found {
			return domain.ErrReplacementStateInvalid
		}
		old, err := q.GetReplacementWorkerNotice(tx, gen.GetReplacementWorkerNoticeParams{OperationID: id, SessionID: string(targetID)})
		if err == nil {
			result, err = decodeNotice(tx, q, op, old)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if result.State == domain.ReplacementNoticeAccepted || result.State == domain.ReplacementNoticeSkipped {
			return nil
		}
		if op.Phase != domain.ReplacementNotifying || op.Revision != revision || op.IntakeReleasedAt == nil || op.IntakeDrainRequestedAt == nil {
			return nil
		}
		if _, _, err := verifiedReplacementIntakeWriter(tx, q, op); err != nil {
			return err
		}
		workerRow, err := q.GetSession(tx, targetID)
		if err != nil {
			return err
		}
		worker := rowToRecord(workerRow)
		if worker.ProjectID != op.ProjectID || worker.Kind != domain.KindWorker || worker.ControllerOwner() != expected {
			return domain.ErrReplacementSourceChanged
		}
		mode := domain.NormalizeSessionMode(worker.Mode)
		if worker.IsTerminated || worker.HibernatedAt != nil || worker.ProvisionState != domain.SessionProvisionReady {
			return nil
		}
		conversation := ""
		if mode == domain.SessionModeChat {
			c, err := q.SelectConversationBySession(tx, &targetID)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			if c.Scope != domain.ConversationScopeSession || c.SessionID == nil || *c.SessionID != targetID || c.ProjectID == nil || *c.ProjectID != op.ProjectID {
				return domain.ErrReplacementStateInvalid
			}
			conversation = c.ID
		} else if worker.Activity.State != domain.ActivityIdle || worker.Metadata.RuntimeLaunchID == "" || worker.Metadata.AgentSessionID == "" || worker.Metadata.AgentSessionIDLaunchID != worker.Metadata.RuntimeLaunchID || worker.Metadata.RuntimeReleasedLaunchID == worker.Metadata.RuntimeLaunchID {
			return nil
		}
		if result.State == domain.ReplacementNoticeUncertain {
			if mode != domain.SessionModeChat || result.Mode != mode || result.ConversationID != conversation {
				return nil
			}
			if result.Owner == worker.ControllerOwner() {
				send = true
				return nil
			}
			// The same canonical conversation owns the unique ContactID across
			// controller epochs. Rebind the private attempt, never the queue key.
		}
		count, err := q.CountReplacementWorkerNoticeRetries(tx, gen.CountReplacementWorkerNoticeRetriesParams{OperationID: id, SessionID: string(targetID)})
		if err != nil {
			return err
		}
		// Terminal receipts have reserved capacity outside this retry budget.
		if count >= replacementNoticeRetryLimit {
			return domain.ErrReplacementNoticeCapacity
		}
		if result.Revision == 0 {
			result = domain.ReplacementNoticeReceipt{OperationID: id, SessionID: targetID, ContactID: target.ContactID, CreatedAt: target.CreatedAt, PayloadHash: noticePayloadHash(op)}
		}
		now := at.UTC()
		result.State, result.Owner, result.Mode, result.AttemptID, result.AttemptedAt, result.ConversationID = domain.ReplacementNoticeUncertain, worker.ControllerOwner(), mode, uuid.NewString(), &now, conversation
		result.ErrorCode, result.SettledAt, result.OutcomeRecordedAt = "", nil, nil
		result, err = saveNotice(tx, q, result, at)
		if err == nil {
			send = true
		}
		return err
	})
	return result, send && err == nil, err
}

// FinishReplacementWorkerNotice cannot mint accepted evidence. A guarded
// no-write may return to pending; any attempted TUI write remains uncertain
// until a matching, scoped native checkpoint is observed.
func (s *Store) FinishReplacementWorkerNotice(ctx context.Context, id string, targetID domain.SessionID, attemptID string, revision int64, attempted bool, at time.Time) (bool, error) {
	saved := false
	if at.IsZero() {
		return false, domain.ErrReplacementStateInvalid
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
		old, err := q.GetReplacementWorkerNotice(tx, gen.GetReplacementWorkerNoticeParams{OperationID: id, SessionID: string(targetID)})
		if err != nil {
			return err
		}
		receipt, err := decodeNotice(tx, q, op, old)
		if err != nil {
			return err
		}
		if receipt.AttemptID != attemptID || receipt.Revision != revision || receipt.State != domain.ReplacementNoticeUncertain || receipt.OutcomeRecordedAt != nil {
			return nil
		}
		// This callback records the outcome of an already authorized attempt,
		// not new delivery authority. Retain positive no-write even after the
		// operation or worker changes; Begin/Validate still fence new writes.
		if !attempted {
			receipt.State, receipt.ErrorCode = domain.ReplacementNoticePending, "DELIVERY_NOT_ATTEMPTED"
		} else {
			receipt.State, receipt.ErrorCode = domain.ReplacementNoticeUncertain, "DELIVERY_UNCONFIRMED"
		}
		now := at.UTC()
		receipt.OutcomeRecordedAt = &now
		if _, err := saveNotice(tx, q, receipt, at); err != nil {
			return err
		}
		saved = true
		return nil
	})
	return saved && err == nil, err
}

// ValidateReplacementWorkerNotice reads a sealed claim under the writer lock.
// The private attempt is not an API credential; only the internal transport
// receives it. Callers still hold the worker's own input/controller lease.
func (s *Store) ValidateReplacementWorkerNotice(ctx context.Context, id string, targetID domain.SessionID, attemptID string, revision int64) (domain.OrchestratorReplacement, domain.SessionRecord, domain.ReplacementNoticeReceipt, error) {
	var op domain.OrchestratorReplacement
	var worker domain.SessionRecord
	var receipt domain.ReplacementNoticeReceipt
	err := s.replacementTx(ctx, func(tx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(tx, id)
		if err != nil {
			return err
		}
		op, err = s.decodeCheckpointedReplacement(tx, q, row)
		if err != nil {
			return err
		}
		if op.Phase != domain.ReplacementNotifying || op.IntakeDrainRequestedAt == nil {
			return domain.ErrReplacementStateInvalid
		}
		if _, _, err := verifiedReplacementIntakeWriter(tx, q, op); err != nil {
			return err
		}
		notice, err := q.GetReplacementWorkerNotice(tx, gen.GetReplacementWorkerNoticeParams{OperationID: id, SessionID: string(targetID)})
		if err != nil {
			return err
		}
		receipt, err = decodeNotice(tx, q, op, notice)
		if err != nil {
			return err
		}
		if attemptID == "" || receipt.AttemptID != attemptID || receipt.Revision != revision || receipt.State != domain.ReplacementNoticeUncertain {
			return domain.ErrReplacementStateInvalid
		}
		rec, err := q.GetSession(tx, targetID)
		if err != nil {
			return err
		}
		worker = rowToRecord(rec)
		if worker.ProjectID != op.ProjectID || worker.Kind != domain.KindWorker || worker.IsTerminated || worker.HibernatedAt != nil || worker.ProvisionState != domain.SessionProvisionReady || worker.ControllerOwner() != receipt.Owner {
			return domain.ErrReplacementSourceChanged
		}
		return nil
	})
	return op, worker, receipt, err
}
