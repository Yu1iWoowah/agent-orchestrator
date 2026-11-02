package agentsettings

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type settingsReconciliationReader interface {
	ListAgentSettingsReconciliations(context.Context, domain.SessionID) ([]domain.AgentSettingsReconciliation, error)
	PendingChatDeliveries(context.Context, domain.SessionID) ([]domain.ChatDeliveryAttempt, error)
}

func (s *Service) readReconciliations(ctx context.Context, snapshot *Snapshot) error {
	if wakes, ok := s.store.(interface {
		ListChatWakeAttempts(context.Context, domain.SessionID) ([]domain.ChatWakeAttempt, error)
	}); ok {
		var err error
		snapshot.WakeAttempts, err = wakes.ListChatWakeAttempts(ctx, snapshot.SessionID)
		if err != nil {
			return err
		}
	}

	reader, ok := s.store.(settingsReconciliationReader)
	if !ok {
		return nil
	}
	var err error
	snapshot.Reconciliations, err = reader.ListAgentSettingsReconciliations(ctx, snapshot.SessionID)
	if err != nil {
		return err
	}
	snapshot.PendingDeliveries, err = reader.PendingChatDeliveries(ctx, snapshot.SessionID)
	return err
}
