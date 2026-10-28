package store

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// ConversationNativeThreadEvidence reads ownership from the same archive that
// committed with each projection. It also works for legacy turns whose rows do
// not retain the provider's native thread identity.
func (s *Store) ConversationNativeThreadEvidence(ctx context.Context, conversationID string, sessionID domain.SessionID) ([]domain.ConversationNativeThreadEvidence, error) {
	rows, err := s.qr.SelectConversationNativeThreadEvidence(ctx, gen.SelectConversationNativeThreadEvidenceParams{
		ConversationID: conversationID, SessionID: sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("read native thread evidence: %w", err)
	}
	evidence := make([]domain.ConversationNativeThreadEvidence, 0, len(rows))
	for _, row := range rows {
		evidence = append(evidence, domain.ConversationNativeThreadEvidence{
			BranchID: row.BranchID, ProviderTurnID: row.ProviderTurnID, ProviderConversationID: row.ProviderConversationID,
		})
	}
	return evidence, nil
}
