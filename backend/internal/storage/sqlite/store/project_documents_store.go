package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// GetProjectDocumentState reads durable ownership and adoption facts.
func (s *Store) GetProjectDocumentState(ctx context.Context, id domain.ProjectID) (domain.ProjectDocumentState, error) {
	row, err := s.projectDocumentReader(ctx).GetProjectDocumentState(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ProjectDocumentState{}, nil
	}
	if err != nil {
		return domain.ProjectDocumentState{}, err
	}
	var state domain.ProjectDocumentState
	if err := json.Unmarshal([]byte(row.StateJson), &state); err != nil {
		return state, err
	}
	state.Revision = row.Revision
	return state, nil
}

// SaveProjectDocumentState compares the revision before persisting new ownership facts.
func (s *Store) SaveProjectDocumentState(ctx context.Context, id domain.ProjectID, state domain.ProjectDocumentState) (bool, error) {
	b, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	q, unlock, err := s.projectDocumentWriter(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	var count int64
	if state.Revision == 0 {
		count, err = q.CreateProjectDocumentState(ctx, gen.CreateProjectDocumentStateParams{ProjectID: string(id), StateJson: string(b)})
	} else {
		count, err = q.UpdateProjectDocumentState(ctx, gen.UpdateProjectDocumentStateParams{ProjectID: string(id), StateJson: string(b), Revision: state.Revision})
	}
	return count == 1, err
}

// projectDocumentPublicationKey binds the adoption completion save to the same
// reservation as its immutable receipt. It is private to this store adapter.
type projectDocumentPublicationKey struct{}

// WithReplacementDocumentPublication binds a daemon-internal document change to
// the active operation and writer. The callback shares both operation and lease
// CAS with the file publication reservation.
func (s *Store) WithReplacementDocumentPublication(ctx context.Context, id string, operationRevision int64, expected domain.SessionRecord, documentRevision int64, publish func(context.Context) error) (bool, error) {
	published := false
	err := s.replacementTx(ctx, func(tx context.Context, q *gen.Queries) error {
		row, err := q.GetOrchestratorReplacement(tx, id)
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
		if op.Revision != operationRevision || op.Phase.Terminal() || expected.ProjectID != op.ProjectID {
			return nil
		}
		if op.SourceRetiredAt == nil {
			switch op.Phase {
			case domain.ReplacementWaitingSourceIdle, domain.ReplacementAdoptingSharedFiles, domain.ReplacementPreparingHandoff, domain.ReplacementHandoffVerified, domain.ReplacementFreezingSharedWrites, domain.ReplacementExportingTranscript, domain.ReplacementTranscriptVerified, domain.ReplacementRetiring:
			default:
				return nil
			}
			if expected.ID != op.SourceSessionID || expected.ControllerOwner() != op.SourceOwner || expected.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
				return nil
			}
		} else {
			matched, err := replacementActivationMatches(tx, q, op, expected)
			if err != nil || !matched {
				return err
			}
		}
		if op.ProjectFingerprint != "" {
			project, err := q.GetProject(tx, op.ProjectID)
			if err != nil {
				return err
			}
			fingerprint, err := domain.ReplacementProjectFingerprint(projectRowFromGen(project))
			if err != nil {
				return err
			}
			if fingerprint != op.ProjectFingerprint {
				return nil
			}
		}
		ok, err := projectDocumentWriterMatches(tx, q, op.ProjectID, expected, documentRevision, true)
		if err != nil || !ok {
			return err
		}
		if err := publish(context.WithValue(tx, projectDocumentPublicationKey{}, q)); err != nil {
			return err
		}
		published = true
		return nil
	})
	return published && err == nil, err
}

func (s *Store) projectDocumentReader(ctx context.Context) *gen.Queries {
	if q, ok := ctx.Value(projectDocumentPublicationKey{}).(*gen.Queries); ok {
		return q
	}
	return s.qr
}
func (s *Store) projectDocumentWriter(ctx context.Context) (*gen.Queries, func(), error) {
	if q, ok := ctx.Value(projectDocumentPublicationKey{}).(*gen.Queries); ok {
		return q, func() {}, nil
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return nil, nil, err
	}
	return s.qw, s.writeMu.Unlock, nil
}

// WithProjectDocumentPublication reserves the SQLite writer until publish returns.
// BEGIN IMMEDIATE also excludes other connections/processes, including controller
// generation, capability, termination and lease updates. No DB lock escapes this
// adapter. The callback must only publish files and optionally save document state.
func (s *Store) WithProjectDocumentPublication(ctx context.Context, id domain.ProjectID, expected domain.SessionRecord, revision int64, publish func(context.Context) error) (bool, error) {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	conn, err := s.writeDB.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()
	q := gen.New(conn)
	matched, err := projectDocumentWriterMatches(ctx, q, id, expected, revision, false)
	if err != nil || !matched {
		return false, err
	}
	if err := publish(context.WithValue(ctx, projectDocumentPublicationKey{}, q)); err != nil {
		return false, err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err == nil, err
}

func projectDocumentWriterMatches(ctx context.Context, q *gen.Queries, id domain.ProjectID, expected domain.SessionRecord, revision int64, allowMissing bool) (bool, error) {
	row, err := q.GetSession(ctx, expected.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	current := rowToRecord(row)
	if current.ProjectID != id || current.IsTerminated || current.Kind != domain.KindOrchestrator || current.ControllerOwner() != expected.ControllerOwner() || current.Metadata.BrowserCapabilityVerifier != expected.Metadata.BrowserCapabilityVerifier {
		return false, nil
	}
	sessions, err := q.ListSessionsByProject(ctx, optionalProjectID(id))
	if err != nil {
		return false, err
	}
	for _, other := range mapListSessionsByProjectRows(sessions) {
		if other.Kind == domain.KindOrchestrator && !other.IsTerminated && other.ID != expected.ID {
			return false, nil
		}
	}
	state, err := q.GetProjectDocumentState(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return allowMissing && revision == 0, ctx.Err()
	}
	if err != nil {
		return false, err
	}
	if state.Revision != revision {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, nil
}
