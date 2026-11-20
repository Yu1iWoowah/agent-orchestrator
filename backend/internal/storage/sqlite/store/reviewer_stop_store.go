package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

var errReviewerStopChanged = errors.New("reviewer stop receipt or launch changed")

// This transaction owns its commit. The caller can stop processes as soon as it returns.
func (s *Store) writeReviewerStop(ctx context.Context, write func(*gen.Queries) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := write(s.txQueries(tx)); err != nil {
		return err
	}
	return tx.Commit()
}

// BeginReviewerStop records original runtime, account, and process facts before a stop.
func (s *Store) BeginReviewerStop(ctx context.Context, receipt domain.ReviewerStopReceipt) error {
	var scope string
	if receipt.ChatScope != nil {
		data, err := json.Marshal(receipt.ChatScope)
		if err != nil {
			return err
		}
		scope = string(data)
	}
	return s.writeReviewerStop(ctx, func(q *gen.Queries) error {
		n, err := q.BeginReviewerStop(ctx, gen.BeginReviewerStopParams{
			ReviewID: receipt.ReviewID, LaunchID: receipt.LaunchID, HandleID: receipt.HandleID,
			ControllerGeneration: receipt.ControllerGeneration, DataRoot: receipt.DataRoot, BootID: receipt.BootID,
			PidNamespace: receipt.PIDNamespace, ChatScope: scope, State: string(receipt.State), Reason: receipt.Reason,
		})
		if err != nil {
			return err
		}
		if n != 1 {
			return errReviewerStopChanged
		}
		if err := q.SnapshotReviewerStopAccounts(ctx, gen.SnapshotReviewerStopAccountsParams{
			ReviewID: receipt.ReviewID, LaunchID: receipt.LaunchID,
		}); err != nil {
			return err
		}
		return writeReviewerStopProcesses(ctx, q, receipt.ReviewerStopKey, receipt.Processes)
	})
}

func writeReviewerStopProcesses(ctx context.Context, q *gen.Queries, key domain.ReviewerStopKey, processes []domain.ReviewerStopProcess) error {
	for _, process := range processes {
		if err := q.AddReviewerStopProcess(ctx, gen.AddReviewerStopProcessParams{
			ReviewID: key.ReviewID, LaunchID: key.LaunchID, Pid: int64(process.PID),
			StartIdentity: process.Start, StartedAt: sql.NullTime{Time: process.StartedAt, Valid: !process.StartedAt.IsZero()},
		}); err != nil {
			return err
		}
	}
	return nil
}

// UpdateReviewerStop persists new lineage and advances the capture state atomically.
func (s *Store) UpdateReviewerStop(ctx context.Context, receipt domain.ReviewerStopReceipt, next domain.ReviewerStopState, reason string, processes []domain.ReviewerStopProcess) error {
	var scope sql.NullString
	if receipt.State == domain.ReviewerStopUnknown && next == domain.ReviewerStopCapturing && receipt.ChatScope != nil {
		data, err := json.Marshal(receipt.ChatScope)
		if err != nil {
			return err
		}
		scope = sql.NullString{String: string(data), Valid: true}
	}
	return s.writeReviewerStop(ctx, func(q *gen.Queries) error {
		n, err := q.UpdateReviewerStopReceipt(ctx, gen.UpdateReviewerStopReceiptParams{
			ReviewID: receipt.ReviewID, LaunchID: receipt.LaunchID, PreviousState: string(receipt.State), NextState: string(next), Reason: reason,
			ChatScope: scope,
		})
		if err != nil {
			return err
		}
		if n != 1 {
			return errReviewerStopChanged
		}
		return writeReviewerStopProcesses(ctx, q, receipt.ReviewerStopKey, processes)
	})
}

// ListReviewerStopReceipts returns all unconfirmed stops, including historical ones.
func (s *Store) ListReviewerStopReceipts(ctx context.Context) ([]domain.ReviewerStopReceipt, error) {
	rows, err := s.qr.ListReviewerStopReceipts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ReviewerStopReceipt, 0, len(rows))
	for _, row := range rows {
		receipt := domain.ReviewerStopReceipt{
			ReviewerStopKey: domain.ReviewerStopKey{ReviewID: row.ReviewID, LaunchID: row.LaunchID},
			SessionID:       domain.SessionID(row.SessionID), HandleID: row.HandleID,
			InterfaceMode: domain.ReviewerInterfaceMode(row.InterfaceMode), ControllerGeneration: row.ControllerGeneration,
			DataRoot: row.DataRoot, BootID: row.BootID, PIDNamespace: row.PidNamespace,
			State: domain.ReviewerStopState(row.State), Reason: row.Reason,
		}
		if row.ChatScope != "" {
			receipt.ChatScope = new(domain.CompactionProcessScope)
			if err := json.Unmarshal([]byte(row.ChatScope), receipt.ChatScope); err != nil {
				return nil, err
			}
		}
		out = append(out, receipt)
	}
	return out, nil
}

// ListReviewerStopProcesses loads the process identities captured for one stop.
func (s *Store) ListReviewerStopProcesses(ctx context.Context, key domain.ReviewerStopKey) ([]domain.ReviewerStopProcess, error) {
	rows, err := s.qr.ListReviewerStopProcesses(ctx, gen.ListReviewerStopProcessesParams{ReviewID: key.ReviewID, LaunchID: key.LaunchID})
	if err != nil {
		return nil, err
	}
	out := make([]domain.ReviewerStopProcess, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.ReviewerStopProcess{PID: int(row.Pid), Start: row.StartIdentity, StartedAt: row.StartedAt.Time})
	}
	return out, nil
}

// SettleReviewerStop clears only the original launch and releases its receipt in one transaction.
func (s *Store) SettleReviewerStop(ctx context.Context, receipt domain.ReviewerStopReceipt, now time.Time) error {
	return s.writeReviewerStop(ctx, func(q *gen.Queries) error {
		if err := q.ClearSettledReviewerRuntime(ctx, gen.ClearSettledReviewerRuntimeParams{
			ReviewID: receipt.ReviewID, LaunchID: receipt.LaunchID, HandleID: receipt.HandleID, ControllerGeneration: receipt.ControllerGeneration,
		}); err != nil {
			return err
		}
		if err := q.CancelSettledReviewerRuns(ctx, gen.CancelSettledReviewerRunsParams{
			ReviewID: receipt.ReviewID, LaunchID: receipt.LaunchID, EndedAt: sql.NullTime{Time: now, Valid: true},
		}); err != nil {
			return err
		}
		n, err := q.DeleteReviewerStopReceipt(ctx, gen.DeleteReviewerStopReceiptParams{
			ReviewID: receipt.ReviewID, LaunchID: receipt.LaunchID, State: string(receipt.State),
		})
		if err != nil {
			return err
		}
		if n != 1 {
			return errReviewerStopChanged
		}
		return nil
	})
}

// HasReviewerStop reports whether this review launch is still protected.
func (s *Store) HasReviewerStop(ctx context.Context, key domain.ReviewerStopKey) (bool, error) {
	return s.qr.HasReviewerStop(ctx, gen.HasReviewerStopParams{ReviewID: key.ReviewID, LaunchID: key.LaunchID})
}

// HasUnsealedReviewerStop reports whether a session still holds source evidence.
func (s *Store) HasUnsealedReviewerStop(ctx context.Context, sessionID domain.SessionID) (bool, error) {
	return s.qr.HasUnsealedReviewerStop(ctx, string(sessionID))
}
