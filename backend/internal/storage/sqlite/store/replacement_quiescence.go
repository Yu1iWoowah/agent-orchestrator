package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

type replacementUnattemptedStopKey struct{}

// ClearUnattemptedReplacementTUIRelease is called only before Destroy was
// attempted. A stale owner/operation cannot retract another launch's marker.
func (s *Store) ClearUnattemptedReplacementTUIRelease(ctx context.Context, operation string, revision int64, id domain.SessionID, launch string) (bool, error) {
	applied := false
	err := s.replacementTx(ctx, func(tx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(tx, operation)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		op, err := s.decodeCheckpointedReplacement(tx, q, row)
		if err != nil {
			return err
		}
		if op.Revision != revision || op.Phase != domain.ReplacementExportingTranscript || op.SourceSessionID != id || op.SourceStopIntentAt == nil || op.SourceStop != nil || op.SourceRetiredAt != nil || op.FrozenAt == nil || launch == "" {
			return nil
		}
		current, err := q.GetSession(tx, id)
		if err != nil {
			return err
		}
		rec := rowToRecord(current)
		if rec.Kind != domain.KindOrchestrator || rec.ProjectID != op.ProjectID || domain.NormalizeSessionMode(rec.Mode) != domain.SessionModeTUI || rec.ControllerOwner() != op.SourceOwner || rec.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier || rec.Activity.State == domain.ActivityExited || rec.HibernatedAt != nil || rec.Metadata.RuntimeLaunchID != launch || rec.Metadata.AgentSessionIDLaunchID != launch {
			return nil
		}
		if rec.Metadata.RuntimeReleasedLaunchID == "" {
			applied = true
			return nil
		}
		count, err := q.ClearUnattemptedReplacementTUIRelease(tx, gen.ClearUnattemptedReplacementTUIReleaseParams{ID: id, ExpectedRevision: current.Revision, LaunchID: launch})
		applied = count == 1
		return err
	})
	return applied && err == nil, err
}

// ClearReplacementStopIntent accepts only the coordinator's positive no-stop
// observation. A generic save cannot clear an intent or an existing stop receipt.
func (s *Store) ClearReplacementStopIntent(ctx context.Context, id string, revision int64, observed ports.ReplacementSourceObservation, at time.Time) (domain.OrchestratorReplacement, bool, error) {
	var result domain.OrchestratorReplacement
	applied := false
	err := s.replacementTx(ctx, func(tx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		op, err := s.decodeCheckpointedReplacement(tx, q, row)
		if err != nil {
			return err
		}
		result = op
		native := op.SourceOwner.ProviderConversationID
		if op.SourceOwner.Mode != domain.SessionModeChat {
			native = op.SourceOwner.AgentSessionID
		}
		if op.Revision != revision || op.Phase != domain.ReplacementExportingTranscript || op.SourceStopIntentAt == nil || op.SourceStop != nil || op.SourceRetiredAt != nil || at.IsZero() || observed.Owner != op.SourceOwner || observed.NativeID == "" || observed.NativeID != native || observed.Provenance != domain.NativeThreadMain || observed.ObservedAt.IsZero() || (observed.State != ports.ChatControllerReady && observed.State != ports.ChatControllerBusy) {
			return nil
		}
		sourceRow, err := q.GetSession(tx, op.SourceSessionID)
		if err != nil {
			return err
		}
		source := rowToRecord(sourceRow)
		if source.IsTerminated || source.ControllerOwner() != op.SourceOwner || source.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier || source.HibernatedAt != nil || source.Activity.State == domain.ActivityExited {
			return nil
		}
		op.SourceStopIntentAt = nil
		op.Error = &domain.ReplacementFailure{Code: "ORCHESTRATOR_HANDOFF_SOURCE_BUSY", Message: "Source has work or approvals; no stop was attempted", Retryable: true}
		op.ResumePhase = domain.ReplacementExportingTranscript
		if !at.Before(op.DeadlineAt) {
			op.Phase = domain.ReplacementAwaitingSource
			op.Error = &domain.ReplacementFailure{Code: "ORCHESTRATOR_HANDOFF_SOURCE_IDLE_TIMEOUT", Message: "Source did not become quiescent before the deadline", Retryable: true}
		}
		op.UpdatedAt = at.UTC()
		saved, err := s.SaveOrchestratorReplacement(context.WithValue(tx, replacementUnattemptedStopKey{}, q), op)
		if err != nil || !saved {
			return err
		}
		op.Revision++
		result, applied = op, true
		return nil
	})
	return result, applied && err == nil, err
}

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
		op, err := s.decodeCheckpointedReplacement(tx, q, row)
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
		op, err := s.decodeCheckpointedReplacement(tx, q, row)
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
