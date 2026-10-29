package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// RecordTurnSettingsSnapshot records wire preferences before provider I/O. It
// does not authorize a dispatch: the caller still owns the controller send gate.
// A missing revision is meaningful for legacy choices that cannot be qualified.
func (s *Store) RecordTurnSettingsSnapshot(ctx context.Context, conversationID, generation string, turnIDs []string, snapshot domain.AgentSettingsTurnSnapshot) error {
	if len(turnIDs) == 0 || snapshot.State != domain.AgentSettingKnown || snapshot.Source != "root" || snapshot.Settings == nil || snapshot.Settings.Validate() != nil || snapshot.CapturedAt == nil || snapshot.CapturedAt.IsZero() || snapshot.Owner == nil || generation == "" {
		return ports.ErrAgentSettingsInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "record dispatch settings", func(q *gen.Queries) error {
		conversation, err := q.SelectConversationByID(ctx, conversationID)
		if err != nil {
			return settingsStoreError(err)
		}
		owner := *snapshot.Owner
		if owner.ControllerGeneration != generation || conversation.CurrentSessionID == nil || *conversation.CurrentSessionID != owner.SessionID || conversation.CurrentReviewID.Valid || conversation.ActiveBranchID != owner.BranchID {
			return fmt.Errorf("%w: dispatch_conversation_owner_changed", ports.ErrAgentSettingsConflict)
		}
		current, err := q.GetSessionAgentSettings(ctx, owner.SessionID)
		if err != nil {
			return settingsStoreError(err)
		}
		if current.IsTerminated || current.SessionMode != domain.SessionModeChat || current.ControllerGeneration != generation {
			return fmt.Errorf("%w: dispatch_controller_lease_changed", ports.ErrAgentSettingsConflict)
		}
		snapshot.Reason = "settings_revision_unavailable"
		if snapshot.Revision != nil {
			if !settingsRecoveryMatches(current, domain.AgentSettingsRecovery{Owner: owner, Revision: *snapshot.Revision, Settings: *snapshot.Settings}) {
				return fmt.Errorf("%w: dispatch_revision_changed", ports.ErrAgentSettingsConflict)
			}
			snapshot.Reason = ""
		}
		// The real provider turn ID is bound separately after SendTurn returns.
		snapshot.ProviderTurnID = ""
		for _, id := range turnIDs {
			turn, err := q.SelectConversationTurnByID(ctx, id)
			if err != nil {
				return settingsStoreError(err)
			}
			if turn.ConversationID != conversationID || turn.HandledByReviewID.Valid {
				return fmt.Errorf("%w: dispatch_turn_owner_changed", ports.ErrAgentSettingsConflict)
			}
			snapshot.TurnID = id
			raw, err := json.Marshal(snapshot)
			if err != nil {
				return err
			}
			if turn.AgentSettingsSnapshotJson != "" {
				if turn.AgentSettingsSnapshotJson != string(raw) {
					return ports.ErrAgentSettingsConflict
				}
				continue
			}
			if turn.ProviderTurnID != "" || turn.CompletedAt.Valid || turn.State != domain.TurnStateQueued && turn.State != domain.TurnStateRunning {
				return fmt.Errorf("%w: dispatch_turn_already_sent", ports.ErrAgentSettingsConflict)
			}
			// A claimed queue receipt can predate a project/branch handoff. Keep
			// its historical handler and branch; this snapshot identifies the
			// current dispatcher. Unclaimed receipts cannot cross that boundary.
			if (turn.HandledBySessionID != owner.SessionID || turn.ControllerGeneration != generation || turn.BranchID != "" && turn.BranchID != owner.BranchID) && !turn.PromotionStartedAt.Valid {
				return fmt.Errorf("%w: dispatch_turn_not_claimed", ports.ErrAgentSettingsConflict)
			}
			changed, err := q.RecordTurnSettingsSnapshot(ctx, gen.RecordTurnSettingsSnapshotParams{ID: id, ConversationID: conversationID, Snapshot: string(raw)})
			if err != nil {
				return err
			}
			if changed != 1 {
				return ports.ErrAgentSettingsConflict
			}
		}
		return nil
	})
}
