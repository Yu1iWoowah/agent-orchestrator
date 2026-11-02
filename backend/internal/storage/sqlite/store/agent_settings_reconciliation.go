package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func decodeSettingsReconciliation(row gen.AgentSettingsReconciliation) (domain.AgentSettingsReconciliation, error) {
	var r domain.AgentSettingsReconciliation
	if err := json.Unmarshal([]byte(row.RecordJson), &r); err != nil {
		return r, err
	}
	r.ExecutorID = row.ExecutorID
	if r.ID != row.ID || string(r.SessionID) != row.SessionID || r.Owner.ControllerGeneration != row.OwnerGeneration || r.Revision != row.SettingsRevision || r.Phase != row.Phase || r.Version != row.Version || !r.ClaimUntil.Equal(row.ClaimUntil) {
		return r, ports.ErrAgentSettingsConflict
	}
	return r, r.Validate()
}

func settingsReconciliationOwner(ctx context.Context, q *gen.Queries, r domain.AgentSettingsReconciliation) error {
	current, err := q.GetSessionAgentSettings(ctx, r.SessionID)
	if err != nil {
		return err
	}
	if current.IsTerminated || chatJournalOwner(current) != r.Owner {
		return ports.ErrAgentSettingsConflict
	}
	settings := domain.AgentSettings{Model: domain.AgentModelSelection(current.Model), Effort: domain.AgentEffortSelection(current.Effort), FastMode: domain.AgentFastMode(current.AgentFastMode)}
	if current.AgentSettingsRevision == r.Revision && settings == r.PreferenceAtStart {
		return nil
	}
	// A successful mutation advances preference exactly once. Completing its
	// diagnostic record must not require changing the original attempt revision.
	if r.Phase == "ready" && r.OperationID != "" && current.AgentSettingsRevision == r.Revision+1 {
		row, err := q.GetAgentSettingsOperation(ctx, gen.GetAgentSettingsOperationParams{SessionID: string(r.SessionID), ID: r.OperationID})
		if err != nil {
			return err
		}
		op, err := decodeSettingsOperation(row)
		if err != nil {
			return err
		}
		if op.Phase == domain.AgentSettingsApplied && op.BaseRevision == r.Revision && op.Source == r.PreferenceAtStart && op.Target == settings {
			return nil
		}
	}
	return ports.ErrAgentSettingsConflict
}

// BeginAgentSettingsReconciliation deduplicates one active owner/revision.
// It never treats an expired claim as proof that its provider writer stopped.
func (s *Store) BeginAgentSettingsReconciliation(ctx context.Context, r domain.AgentSettingsReconciliation) (domain.AgentSettingsReconciliation, error) {
	if r.Validate() != nil || r.Version != 0 || r.Phase != "observing" || r.EffectPending {
		return r, ports.ErrAgentSettingsInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return r, err
	}
	defer s.writeMu.Unlock()
	err := s.inTx(ctx, "begin settings reconciliation", func(q *gen.Queries) error {
		if err := settingsReconciliationOwner(ctx, q, r); err != nil {
			return err
		}
		row, err := q.GetActiveSettingsReconciliation(ctx, gen.GetActiveSettingsReconciliationParams{SessionID: string(r.SessionID), OwnerGeneration: r.Owner.ControllerGeneration, SettingsRevision: r.Revision})
		if err == nil {
			active, err := decodeSettingsReconciliation(row)
			if err != nil {
				return err
			}
			if active.ExecutorID != r.ExecutorID || active.OperationID != r.OperationID {
				return ports.ErrAgentSettingsConflict
			}
			r = active
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return q.InsertSettingsReconciliation(ctx, gen.InsertSettingsReconciliationParams{ID: r.ID, SessionID: string(r.SessionID), OwnerGeneration: r.Owner.ControllerGeneration, SettingsRevision: r.Revision, Phase: r.Phase, Version: r.Version, ExecutorID: r.ExecutorID, ClaimUntil: r.ClaimUntil, RecordJson: string(raw), CreatedAt: r.CreatedAt, UpdatedAt: r.LastProgressAt})
	})
	return r, err
}

// SaveAgentSettingsReconciliation checkpoints before I/O and records outcomes
// before CDC. Only the exact claimed executor can advance the work.
func (s *Store) SaveAgentSettingsReconciliation(ctx context.Context, next domain.AgentSettingsReconciliation, expected int64, executor string) (domain.AgentSettingsReconciliation, error) {
	if next.Validate() != nil || expected < 0 || next.Version != expected+1 || next.ExecutorID != executor {
		return next, ports.ErrAgentSettingsInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return next, err
	}
	defer s.writeMu.Unlock()
	err := s.inTx(ctx, "save settings reconciliation", func(q *gen.Queries) error {
		row, err := q.GetSettingsReconciliation(ctx, gen.GetSettingsReconciliationParams{SessionID: string(next.SessionID), ID: next.ID})
		if err != nil {
			return err
		}
		old, err := decodeSettingsReconciliation(row)
		if err != nil {
			return err
		}
		if old.Phase == "ready" || old.Phase == "cancelled" || old.ExecutorID != executor || old.Version != expected || old.Owner != next.Owner || old.Revision != next.Revision || old.Desired != next.Desired || old.PreferenceAtStart != next.PreferenceAtStart || old.RetryOfAttemptID != next.RetryOfAttemptID || old.RetryOfVersion != next.RetryOfVersion || old.OperationID != next.OperationID || !old.CreatedAt.Equal(next.CreatedAt) || next.LastProgressAt.Before(old.LastProgressAt) || next.RetryCount < old.RetryCount {
			return ports.ErrAgentSettingsConflict
		}
		if err := settingsReconciliationOwner(ctx, q, next); err != nil {
			return err
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		n, err := q.UpdateSettingsReconciliation(ctx, gen.UpdateSettingsReconciliationParams{Phase: next.Phase, ExecutorID: executor, ClaimUntil: next.ClaimUntil, RecordJson: string(raw), UpdatedAt: next.LastProgressAt, SessionID: string(next.SessionID), ID: next.ID, ExpectedVersion: expected, ExpectedExecutor: executor})
		if err == nil && n != 1 {
			return ports.ErrAgentSettingsConflict
		}
		return err
	})
	return next, err
}

// ClaimAgentSettingsReconciliation is a versioned takeover of read-only work.
// Uncertain side effects keep the same attempt fenced for outcome resolution.
func (s *Store) ClaimAgentSettingsReconciliation(ctx context.Context, id domain.SessionID, key string, expected int64, executor string, at time.Time) (domain.AgentSettingsReconciliation, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.AgentSettingsReconciliation{}, err
	}
	defer s.writeMu.Unlock()
	var result domain.AgentSettingsReconciliation
	err := s.inTx(ctx, "claim settings reconciliation", func(q *gen.Queries) error {
		row, err := q.GetSettingsReconciliation(ctx, gen.GetSettingsReconciliationParams{SessionID: string(id), ID: key})
		if err != nil {
			return err
		}
		r, err := decodeSettingsReconciliation(row)
		if err != nil {
			return err
		}
		if r.Version != expected || r.EffectPending || r.ClaimUntil.After(at) || r.Phase == "ready" || r.Phase == "cancelled" {
			return ports.ErrAgentSettingsConflict
		}
		if err := settingsReconciliationOwner(ctx, q, r); err != nil {
			return err
		}
		oldExecutor := r.ExecutorID
		r.Version++
		r.ExecutorID, r.ClaimUntil = executor, at.Add(30*time.Second)
		if r.Validate() != nil {
			return ports.ErrAgentSettingsInvalid
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		n, err := q.UpdateSettingsReconciliation(ctx, gen.UpdateSettingsReconciliationParams{Phase: r.Phase, ExecutorID: executor, ClaimUntil: r.ClaimUntil, RecordJson: string(raw), UpdatedAt: at, SessionID: string(id), ID: key, ExpectedVersion: expected, ExpectedExecutor: oldExecutor})
		if err != nil {
			return err
		}
		if n != 1 {
			return ports.ErrAgentSettingsConflict
		}
		result = r
		return nil
	})
	return result, err
}

// ListAgentSettingsReconciliations preserves failure history across restart.
func (s *Store) ListAgentSettingsReconciliations(ctx context.Context, id domain.SessionID) ([]domain.AgentSettingsReconciliation, error) {
	rows, err := s.qr.ListSettingsReconciliations(ctx, string(id))
	if err != nil {
		return nil, err
	}
	result := make([]domain.AgentSettingsReconciliation, 0, len(rows))
	for _, row := range rows {
		r, err := decodeSettingsReconciliation(row)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, nil
}
