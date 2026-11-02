package store

import (
	"context"
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// SaveChatWakeAttempt creates or advances wake diagnostics using version CAS.
func (s *Store) SaveChatWakeAttempt(ctx context.Context, a domain.ChatWakeAttempt, expected int64) (domain.ChatWakeAttempt, error) {
	if a.Validate() != nil {
		return a, ports.ErrAgentSettingsInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return a, err
	}
	defer s.writeMu.Unlock()
	err := s.inTx(ctx, "record chat wake attempt", func(q *gen.Queries) error {
		current, err := q.GetSessionAgentSettings(ctx, a.SessionID)
		if err != nil {
			return err
		}
		if current.IsTerminated {
			return ports.ErrAgentSettingsConflict
		}
		if expected < 0 {
			if a.Version != 0 || a.Phase != "waking" {
				return ports.ErrAgentSettingsInvalid
			}
			a.OwnerGeneration = current.ControllerGeneration
			raw, err := json.Marshal(a)
			if err != nil {
				return err
			}
			return q.InsertChatWakeAttempt(ctx, gen.InsertChatWakeAttemptParams{ID: a.ID, SessionID: string(a.SessionID), Version: a.Version, RecordJson: string(raw)})
		}
		row, err := q.GetChatWakeAttempt(ctx, gen.GetChatWakeAttemptParams{SessionID: string(a.SessionID), ID: a.ID})
		if err != nil {
			return err
		}
		var old domain.ChatWakeAttempt
		if err := json.Unmarshal([]byte(row.RecordJson), &old); err != nil {
			return err
		}
		if old.Validate() != nil || old.Version != expected || old.Phase == "blocked" || old.Phase == "ready" || a.Version != expected+1 || !old.CreatedAt.Equal(a.CreatedAt) || old.OwnerGeneration != a.OwnerGeneration {
			return ports.ErrAgentSettingsConflict
		}
		raw, err := json.Marshal(a)
		if err != nil {
			return err
		}
		n, err := q.UpdateChatWakeAttempt(ctx, gen.UpdateChatWakeAttemptParams{RecordJson: string(raw), SessionID: string(a.SessionID), ID: a.ID, ExpectedVersion: expected})
		if err != nil {
			return err
		}
		if n != 1 {
			return ports.ErrAgentSettingsConflict
		}
		return nil
	})
	return a, err
}

// ListChatWakeAttempts reads durable wake diagnostics in journal order.
func (s *Store) ListChatWakeAttempts(ctx context.Context, id domain.SessionID) ([]domain.ChatWakeAttempt, error) {
	rows, err := s.qr.ListChatWakeAttempts(ctx, string(id))
	if err != nil {
		return nil, err
	}
	result := make([]domain.ChatWakeAttempt, 0, len(rows))
	for _, row := range rows {
		var a domain.ChatWakeAttempt
		if err := json.Unmarshal([]byte(row.RecordJson), &a); err != nil {
			return nil, err
		}
		if err := a.Validate(); err != nil {
			return nil, err
		}
		if a.ID != row.ID || string(a.SessionID) != row.SessionID || a.Version != row.Version {
			return nil, ports.ErrAgentSettingsConflict
		}
		result = append(result, a)
	}
	return result, nil
}
