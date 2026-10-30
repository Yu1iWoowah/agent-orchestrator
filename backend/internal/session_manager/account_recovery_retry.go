package sessionmanager

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// RetryAccountRecovery is explicit permission to retry shutdown of this
// session's failed account controller. It reuses the exact-generation recovery
// and durable receipts; it cannot clear a fence or launch a replacement itself.
func (m *Manager) RetryAccountRecovery(ctx context.Context, id domain.SessionID) (bool, error) {
	if _, found, err := m.store.GetSession(ctx, id); err != nil {
		return false, err
	} else if !found {
		return false, ErrNotFound
	}
	store, ok := m.store.(accountTransitionRecoveryStore)
	if !ok {
		return false, ErrInterfaceHandoffUnsupported
	}
	pending, err := store.ListUnsettledAccountTransitions(ctx)
	if err != nil {
		return false, err
	}
	transitions, ok := m.store.(interfaceTransitionStore)
	if !ok {
		return false, ErrInterfaceHandoffUnsupported
	}
	if active, found, err := transitions.GetActiveSessionInterfaceTransition(ctx, id); err != nil {
		return false, err
	} else if found {
		if !active.AccountMove() || active.ErrorCode != "TARGET_STOP_UNCONFIRMED" {
			return false, ErrInterfaceTransitionInProgress
		}
		pending = append(pending, active)
	}
	for _, receipt := range pending {
		if receipt.SessionID != id {
			continue
		}
		if _, err := m.recoverInterfaceTransitions(ctx, receipt.ID); err != nil {
			return false, err
		}
	}
	// The recovery post-pass checks all sibling receipts before releasing the
	// session fence. Never equate a nil stop error with confirmed shutdown.
	return !m.SessionMutationInProgress(id), nil
}
