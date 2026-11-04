package store

import (
	"context"
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

var _ ports.SideCompactionStore = (*Store)(nil)

func (s *Store) RegisterSideCompaction(ctx context.Context, r domain.SideCompaction) error {
	if r.ID == "" || r.SideID == "" || r.NativeHome == "" || r.NativeID == "" || r.State != domain.SideCompactionPending || r.Historical || r.Closed || !json.Valid(r.Content) {
		return domain.ErrSideCompactionTransition
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "register side compaction", func(q *gen.Queries) error {
		if err := q.InsertSideCompaction(ctx, gen.InsertSideCompactionParams{ID: r.ID, SideID: r.SideID, NativeHome: r.NativeHome, NativeID: r.NativeID, Epoch: r.Epoch, RecordJson: string(raw)}); err != nil {
			return err
		}
		return q.PutSideCompactionContent(ctx, gen.PutSideCompactionContentParams{SideID: r.SideID, ContentJson: string(r.Content)})
	})
}

func (s *Store) SideCompactions(ctx context.Context, unresolved bool) ([]domain.SideCompaction, error) {
	rows, err := s.qr.ListSideCompactions(ctx, unresolved)
	if err != nil {
		return nil, err
	}
	out := make([]domain.SideCompaction, 0, len(rows))
	for _, row := range rows {
		var r domain.SideCompaction
		if err := json.Unmarshal([]byte(row.RecordJson), &r); err != nil {
			return nil, err
		}
		r.State, r.Historical, r.Closed = domain.SideCompactionState(row.State), row.Historical != 0, row.Closed != 0
		r.Content = json.RawMessage(row.ContentJson)
		out = append(out, r)
	}
	return out, nil
}

func (s *Store) SaveSideCompactionContent(ctx context.Context, sideID string, content json.RawMessage) error {
	if !json.Valid(content) {
		return domain.ErrSideCompactionTransition
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.qw.UpdateSideCompactionContent(ctx, gen.UpdateSideCompactionContentParams{SideID: sideID, ContentJson: string(content)})
}

func (s *Store) TransitionSideCompaction(ctx context.Context, id, epoch string, state domain.SideCompactionState, historical, closed bool) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "transition side compaction", func(q *gen.Queries) error {
		row, err := q.GetSideCompaction(ctx, id)
		if err != nil {
			return err
		}
		current := domain.SideCompaction{State: domain.SideCompactionState(row.State), Historical: row.Historical != 0, Closed: row.Closed != 0}
		if row.Epoch != epoch {
			return domain.ErrSideCompactionTransition
		}
		if _, err := current.Transition(state, historical, closed); err != nil {
			return err
		}
		var h, c int64
		if historical {
			h = 1
		}
		if closed {
			c = 1
		}
		if err := q.UpdateSideCompaction(ctx, gen.UpdateSideCompactionParams{ID: id, State: string(state), Historical: h, Closed: c}); err != nil {
			return err
		}
		return q.DeleteSettledSideContent(ctx, row.SideID)
	})
}
