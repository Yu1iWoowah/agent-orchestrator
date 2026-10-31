package chat

import (
	"context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// HistoricalSnapshotReader reads a retired orchestrator without rebinding the
// project narrative or admitting it as a live controller.
type HistoricalSnapshotReader func(context.Context, domain.SessionID, int64, int64) (ConversationRows, error)

func (s *Service) snapshotWithoutCurrentConversation(ctx context.Context, record domain.SessionRecord, beforeSequence, limit int64) (Snapshot, error) {
	snapshot := Snapshot{SessionID: record.ID, Harness: record.Harness, Mode: domain.NormalizeSessionMode(record.Mode), Controller: idleControllerState(record)}
	if !record.IsTerminated || record.Kind != domain.KindOrchestrator || s.historicalReader == nil {
		return snapshot, nil
	}
	rows, err := s.historicalReader(ctx, record.ID, beforeSequence, limit)
	if errors.Is(err, domain.ErrNoConversation) {
		return snapshot, nil
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("historical conversation for %s: %w", record.ID, err)
	}
	for i := range rows.Turns {
		rows.Turns[i].ProviderTurnID = ""
	}
	snapshot.Conversation = domain.ConversationRecord{
		ID: rows.Conversation.ID, Scope: rows.Conversation.Scope, ProjectID: record.ProjectID,
		SessionID: record.ID, ActiveBranchID: rows.ActiveBranch.ID, LatestSequence: rows.Conversation.LatestSequence,
	}
	snapshot.ActiveBranch = rows.ActiveBranch
	snapshot.EditFloorSequence = rows.Conversation.LatestSequence
	snapshot.Controller = ports.ChatControllerStopped
	snapshot.Turns, snapshot.Messages, snapshot.Activities = rows.Turns, rows.Messages, rows.Activities
	snapshot.OldestSequence, snapshot.HasMoreBefore = rows.OldestSequence, rows.HasMoreBefore
	return snapshot, nil
}
