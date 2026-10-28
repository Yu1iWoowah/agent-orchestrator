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
		return false, nil
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
	if err := publish(context.WithValue(ctx, projectDocumentPublicationKey{}, q)); err != nil {
		return false, err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err == nil, err
}
