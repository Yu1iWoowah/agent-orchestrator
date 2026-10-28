package chat

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type nativeThreadEvidenceReader interface {
	ConversationNativeThreadEvidence(context.Context, string, domain.SessionID) ([]domain.ConversationNativeThreadEvidence, error)
}

type nativeThreadTurnKey struct {
	branchID, providerTurnID string
}

func (s *Service) loadNativeThreadCheckpoint(ctx context.Context, checkpoint *nativeHistoryCheckpoint, active domain.ConversationBranch, turns []domain.ConversationTurn, cfg StartConfig) error {
	checkpoint.nativeThreads = make(map[nativeThreadTurnKey]string)
	checkpoint.nativeThreadRoots = map[string]string{active.ID: cfg.ProviderConversationID}
	// Native forks replay their ancestors' main turns as well. A different
	// native thread is a child only relative to the branch that owned its turn.
	for _, turn := range turns {
		if turn.HandledBySessionID != cfg.SessionID || turn.RolledBackAt != nil || turn.State != domain.TurnStateCompleted || turn.ProviderTurnID == "" {
			continue
		}
		if _, found := checkpoint.nativeThreadRoots[turn.BranchID]; found {
			continue
		}
		branch, err := s.store.ConversationBranch(ctx, active.ConversationID, turn.BranchID)
		if err != nil {
			return err
		}
		checkpoint.nativeThreadRoots[branch.ID] = branch.ProviderConversationID
	}
	reader, ok := s.store.(nativeThreadEvidenceReader)
	if !ok {
		// A completed candidate without ownership evidence remains a hard gate.
		return nil
	}
	evidence, err := reader.ConversationNativeThreadEvidence(ctx, active.ConversationID, cfg.SessionID)
	if err != nil {
		return err
	}
	for _, item := range evidence {
		key := nativeThreadTurnKey{item.BranchID, item.ProviderTurnID}
		previous, found := checkpoint.nativeThreads[key]
		if found && previous != item.ProviderConversationID {
			// Once conflicting, a later duplicate must not make ownership known.
			checkpoint.nativeThreads[key] = ""
		} else if !found {
			checkpoint.nativeThreads[key] = item.ProviderConversationID
		}
	}
	return nil
}
