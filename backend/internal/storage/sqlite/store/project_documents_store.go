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
	row, err := s.qr.GetProjectDocumentState(ctx, string(id))
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
	if err := s.writeMu.LockContext(ctx); err != nil {
		return false, err
	}
	defer s.writeMu.Unlock()
	var count int64
	if state.Revision == 0 {
		count, err = s.qw.CreateProjectDocumentState(ctx, gen.CreateProjectDocumentStateParams{ProjectID: string(id), StateJson: string(b)})
	} else {
		count, err = s.qw.UpdateProjectDocumentState(ctx, gen.UpdateProjectDocumentStateParams{ProjectID: string(id), StateJson: string(b), Revision: state.Revision})
	}
	return count == 1, err
}
