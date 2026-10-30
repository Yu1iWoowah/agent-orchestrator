package store

import (
	"context"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func isReplacementNoticeCheckpoint(worker domain.SessionRecord) bool {
	return worker.Kind == domain.KindWorker && domain.NormalizeSessionMode(worker.Mode) == domain.SessionModeTUI && worker.Metadata.ConversationCheckpointState == domain.ConversationCheckpointCoordination && strings.HasPrefix(worker.Metadata.ConversationCheckpointTurnID, "handoff-notice-attempt:")
}

// The hook CAS and positive FYI receipt commit together. A later prompt or
// owner epoch may replace the latest session register without losing delivery.
func recordReplacementNoticeCheckpoint(ctx context.Context, q *gen.Queries, worker domain.SessionRecord) error {
	m := worker.Metadata
	const prefix = "handoff-notice-attempt:"
	if !isReplacementNoticeCheckpoint(worker) {
		return nil
	}
	rows, err := q.FindReplacementWorkerNoticeAttempt(ctx, gen.FindReplacementWorkerNoticeAttemptParams{SessionID: string(worker.ID), AttemptID: strings.TrimPrefix(m.ConversationCheckpointTurnID, prefix)})
	if err != nil {
		return err
	}
	if len(rows) > 1 {
		return domain.ErrReplacementStateInvalid
	}
	if len(rows) == 0 {
		return nil
	}
	receipt, err := decodeStoredNotice(ctx, q, rows[0])
	if err != nil {
		return err
	}
	op, err := q.GetOrchestratorReplacement(ctx, receipt.OperationID)
	if err != nil {
		return err
	}
	if string(worker.ProjectID) != op.ProjectID || receipt.Mode != domain.SessionModeTUI || receipt.Owner != worker.ControllerOwner() || m.RuntimeLaunchID == "" || m.AgentSessionID == "" || m.AgentSessionIDLaunchID != m.RuntimeLaunchID || m.ConversationCheckpointGeneration != m.RuntimeLaunchID || m.ConversationCheckpointNativeID != m.AgentSessionID || m.ConversationCheckpointTurnID != receipt.NativeDeliveryID() {
		return nil
	}
	// This intrinsic, scoped hook fact does not rewrite/decode the operation or
	// release a fence. Completion separately verifies its sealed target/payload.
	now := worker.UpdatedAt.UTC()
	receipt.State, receipt.TurnID, receipt.EvidenceKind, receipt.ErrorCode, receipt.SettledAt = domain.ReplacementNoticeAccepted, receipt.NativeDeliveryID(), "tui_checkpoint", "", &now
	_, err = saveNotice(ctx, q, receipt, now)
	return err
}
