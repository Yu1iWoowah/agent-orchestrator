package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// SetReplacementSessionHibernated atomically checks the operation and current
// controller before recording the shutdown intent. It never starts/stops a host.
func (s *Store) SetReplacementSessionHibernated(ctx context.Context, operation string, revision int64, id domain.SessionID, sessionRevision int64, at time.Time) (bool, error) {
	applied := false
	err := s.replacementTx(ctx, func(tx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(tx, operation)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		op, err := decodeReplacement(row)
		if err != nil {
			return err
		}
		if op.Revision != revision {
			return nil
		}
		current, err := q.GetSession(tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if !op.AllowsChatQuiescence(rowToRecord(current)) || current.Revision != sessionRevision {
			return nil
		}
		count, err := q.SetSessionHibernated(tx, gen.SetSessionHibernatedParams{ID: id, ExpectedRevision: sessionRevision, HibernatedAt: timeToNullTime(at)})
		applied = count == 1
		return err
	})
	return applied && err == nil, err
}

// ReplacementWakeFenced checks intake without reading a stale hibernation
// marker before the Chat service has joined an existing wake attempt.
func (s *Store) ReplacementWakeFenced(ctx context.Context, id domain.SessionID) (bool, error) {
	return s.replacementReader(ctx).ReplacementWakeFenced(ctx, id)
}

// SetReplacementTUIRuntimeReleased reserves the original launch's deliberate
// stop only while operation and observed idle session revisions still match.
func (s *Store) SetReplacementTUIRuntimeReleased(ctx context.Context, operation string, revision int64, id domain.SessionID, sessionRevision int64) (bool, error) {
	applied := false
	err := s.replacementTx(ctx, func(tx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(tx, operation)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		op, err := decodeReplacement(row)
		if err != nil {
			return err
		}
		if op.Revision != revision {
			return nil
		}
		current, err := q.GetSession(tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Revision != sessionRevision || !op.AllowsTUIQuiescence(rowToRecord(current)) {
			return nil
		}
		count, err := q.SetSessionRuntimeReleased(tx, gen.SetSessionRuntimeReleasedParams{ID: id, LaunchID: current.RuntimeLaunchID})
		applied = count == 1
		return err
	})
	return applied && err == nil, err
}
