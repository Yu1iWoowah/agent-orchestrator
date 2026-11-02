package chat

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// RetryAgentSettings is an explicitly authorized retry of observation/work,
// never a second settings intent, input replay, or uncertain native effect.
func (s *Service) RetryAgentSettings(ctx context.Context, id domain.SessionID, operationID, attemptID string, version, revision int64) (domain.AgentSettingsReconciliation, error) {
	c, release, err := s.admitControllerWork(ctx, id, false, controllerIntake)
	if err != nil {
		return domain.AgentSettingsReconciliation{}, err
	}
	defer release()
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	journal, ok := c.store.(settingsReconciliationJournal)
	if !ok {
		return domain.AgentSettingsReconciliation{}, ports.ErrAgentSettingsUnsupported
	}
	attempts, err := journal.ListAgentSettingsReconciliations(ctx, id)
	if err != nil {
		return domain.AgentSettingsReconciliation{}, err
	}
	for _, a := range attempts {
		if a.RetryOfAttemptID == attemptID && a.OperationID == operationID {
			if a.RetryOfVersion != version || a.Revision != revision {
				return domain.AgentSettingsReconciliation{}, ports.ErrAgentSettingsConflict
			}
			return a, nil
		}
	}
	var source *domain.AgentSettingsReconciliation
	for i := range attempts {
		if attempts[i].ID == attemptID {
			source = &attempts[i]
			break
		}
	}
	if source == nil || source.OperationID != operationID || source.Version != version || source.Revision != revision || source.EffectPending || source.Phase == "ready" || source.Phase == "cancelled" {
		return domain.AgentSettingsReconciliation{}, ports.ErrAgentSettingsConflict
	}
	settings, supported := c.store.(agentSettingsJournal)
	if !supported {
		return domain.AgentSettingsReconciliation{}, ports.ErrAgentSettingsUnsupported
	}
	current, err := c.settingsContext(ctx, settings)
	if err != nil {
		return domain.AgentSettingsReconciliation{}, err
	}
	if ownerForSettings(current) != source.Owner || current.Revision != revision {
		return domain.AgentSettingsReconciliation{}, ports.ErrAgentSettingsConflict
	}
	pending, found, pendingErr := settings.PendingAgentSettingsOperation(ctx, id)
	if pendingErr != nil {
		return domain.AgentSettingsReconciliation{}, pendingErr
	}
	if found && pending.ID != operationID {
		return domain.AgentSettingsReconciliation{}, ports.ErrAgentSettingsConflict
	}
	c.settingsAttempt = source
	if err := c.retrySettingsAttemptLocked(ctx); err != nil {
		return domain.AgentSettingsReconciliation{}, err
	}
	c.settingsRetrySourceID = source.ID
	c.settingsRetrySourceVersion = version
	defer func() { c.settingsRetrySourceID = ""; c.settingsRetrySourceVersion = 0 }()
	blocked, runErr := c.reconcileAgentSettingsLocked(ctx)
	if c.settingsAttempt == nil {
		return domain.AgentSettingsReconciliation{}, ports.ErrAgentSettingsConflict
	}
	result := *c.settingsAttempt
	if runErr == nil && !blocked {
		_ = c.drainLocked(ctx, true)
	} else {
		c.queueSettingsRecheck()
	}
	return result, nil
}
