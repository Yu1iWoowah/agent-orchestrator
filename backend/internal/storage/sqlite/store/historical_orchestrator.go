package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// LoadHistoricalSessionSnapshotPage serves the last native branch of a retired
// project orchestrator. The current owner and active branch stay untouched.
func (s *Store) LoadHistoricalSessionSnapshotPage(ctx context.Context, session domain.SessionID, beforeSequence, limit int64) (ConversationSnapshot, error) {
	row, err := s.qr.SelectRetiredOrchestratorConversation(ctx, session)
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationSnapshot{}, ErrConversationNotFound
	}
	if err != nil {
		return ConversationSnapshot{}, fmt.Errorf("select historical orchestrator %s: %w", session, err)
	}
	branch, err := s.ConversationBranch(ctx, row.Conversation.ID, row.ConversationBranch.ID)
	if err != nil {
		return ConversationSnapshot{}, err
	}
	conv := row.Conversation
	// Only the in-memory projection uses the old owner/reset boundary.
	conv.CurrentSessionID, conv.ActiveBranchID = &session, branch.ID
	visibleAfter, err := s.conversationVisibleAfterSequence(ctx, conv)
	if err != nil {
		return ConversationSnapshot{}, err
	}
	nextReset, err := s.qr.SelectConversationNextContextResetSequence(ctx, gen.SelectConversationNextContextResetSequenceParams{ConversationID: conv.ID, AfterSequence: visibleAfter})
	if err != nil {
		return ConversationSnapshot{}, err
	}
	if nextReset > 0 && nextReset <= conv.LatestSequence {
		conv.LatestSequence = nextReset - 1
	}
	presentation := conversationHistoryPresentation{activeBranch: branch, editFloorSequence: conv.LatestSequence}
	return s.loadConversationSnapshotPage(ctx, conv, branch.ID, presentation, beforeSequence, limit)
}
