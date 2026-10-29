package chat

import (
	"context"
	"reflect"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func cloneSideSettings(turn domain.SideTurn) domain.SideTurn {
	if turn.SettingsAttempts != nil {
		frames := make([]domain.AgentSettingsTurnSnapshot, len(turn.SettingsAttempts))
		for i := range frames {
			frames[i] = turn.SettingsAttempts[i].Clone()
		}
		turn.SettingsAttempts = frames
	}
	return turn
}

// RecordSideTurnSettings retains each attempt in the existing process-local
// recovery store. It never creates durable root session/conversation rows.
func (m *memorySideStore) RecordSideTurnSettings(ctx context.Context, sideID, turnID, generation string, frame domain.AgentSettingsTurnSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	side, ok := m.sides[sideID]
	if !ok || side.ClosedAt != nil || side.Generation != generation {
		return ErrSideClosed
	}
	if frame.State != domain.AgentSettingKnown || frame.Source != "side" || frame.Revision != nil || frame.Owner == nil || frame.Owner.SessionID != domain.SessionID(side.ProviderHostID) || frame.Owner.ControllerGeneration != generation || frame.Owner.BranchID != sideID || frame.Owner.ProviderConversationID == "" || frame.Settings == nil || *frame.Settings != (domain.AgentSettings{Model: domain.AgentModelSelection(side.Model), Effort: domain.AgentEffortSelection(side.Effort), FastMode: domain.AgentFastDefault}) || frame.TurnID != turnID || frame.CapturedAt == nil || frame.CapturedAt.IsZero() {
		return ErrSideClosed
	}
	for i := range m.turns[sideID] {
		turn := &m.turns[sideID][i]
		if turn.ID != turnID {
			continue
		}
		if turn.State != "running" || turn.ProviderTurnID != "" || frame.Attempt != turn.DispatchAttempt {
			return ErrSideClosed
		}
		for _, saved := range turn.SettingsAttempts {
			if saved.Attempt != frame.Attempt {
				continue
			}
			frame.CapturedAt = saved.CapturedAt
			if !reflect.DeepEqual(saved, frame) {
				return ErrSideIdempotencyConflict
			}
			return nil
		}
		turn.SettingsAttempts = append(turn.SettingsAttempts, frame.Clone())
		return nil
	}
	return ErrSideClosed
}
