package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

var errSettingsOwnerUnrecoverable = fmt.Errorf("%w: settings_owner_unrecoverable", ports.ErrAgentSettingsConflict)

// Only fully identified Chat owners currently have a restoration protocol.
// Gate the first provider action on the same eligibility used for recovery.
func settingsOwnerCanRecover(row gen.GetSessionAgentSettingsRow) bool {
	return !row.IsTerminated && row.SessionMode == domain.SessionModeChat &&
		row.ControllerGeneration != "" && row.AccountID != "" && row.ProviderConversationID != "" && row.BranchID != ""
}

func settingsRecoveryMatches(row gen.GetSessionAgentSettingsRow, recovery domain.AgentSettingsRecovery) bool {
	owner := domain.AgentSettingsOwner{
		SessionID: row.ID, ControllerGeneration: row.ControllerGeneration,
		AccountID: row.AccountID, ProviderConversationID: row.ProviderConversationID, BranchID: row.BranchID,
	}
	settings := domain.AgentSettings{Model: domain.AgentModelSelection(row.Model), Effort: domain.AgentEffortSelection(row.Effort), FastMode: domain.AgentFastMode(row.AgentFastMode)}
	return settingsOwnerCanRecover(row) &&
		recovery.Owner == owner && recovery.Revision == row.AgentSettingsRevision && recovery.Settings == settings
}

// AdvanceAgentSettingsRecovery records a current executor before restoration.
// The runtime caller must verify old-writer shutdown and the restoration itself;
// this persistence primitive supplies CAS fencing, not provider or actor authority.
// A crash may leave a started pending operation without a recovery phase yet.
func (s *Store) AdvanceAgentSettingsRecovery(ctx context.Context, id domain.SessionID, operationID string, expectedVersion int64, checkpoint domain.AgentSettingsCheckpoint, expected domain.AgentSettingsRecovery, at time.Time) (domain.AgentSettingsOperation, error) {
	if at.IsZero() || expectedVersion < 0 || expected.Settings.Validate() != nil || expected.Revision < 0 || (checkpoint != domain.AgentSettingsRollingBack && checkpoint != domain.AgentSettingsRestored) {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	defer s.writeMu.Unlock()
	var result domain.AgentSettingsOperation
	err := s.inTx(ctx, "advance settings recovery", func(q *gen.Queries) error {
		row, err := q.GetAgentSettingsOperation(ctx, gen.GetAgentSettingsOperationParams{SessionID: string(id), ID: operationID})
		if err != nil {
			return settingsStoreError(err)
		}
		op, err := decodeSettingsOperation(row)
		if err != nil {
			return err
		}
		if op.Version != expectedVersion || op.StartedAt == nil || (op.Phase != domain.AgentSettingsPending && op.Phase != domain.AgentSettingsRecoveryRequired) {
			return ports.ErrAgentSettingsConflict
		}
		current, err := q.GetSessionAgentSettings(ctx, id)
		if err != nil {
			return settingsStoreError(err)
		}
		if !settingsOperationRecoveryMatches(current, op, expected) {
			return ports.ErrAgentSettingsConflict
		}
		if op.TUI != nil {
			if expected.Plan == nil || expected.Qualification == nil || expected.Qualification.InputFingerprint == "" || expected.Qualification.PolicyVersion == "" || expected.Plan.Source != expected.Settings || expected.Plan.Target != expected.Settings || expected.Plan.ProviderConversationID != op.ProviderConversationID || expected.Plan.Before.Model == "" || expected.Plan.After.Model == "" {
				return ports.ErrAgentSettingsInvalid
			}
		}
		if checkpoint == domain.AgentSettingsRestored {
			if op.Phase != domain.AgentSettingsRecoveryRequired || op.Checkpoint != domain.AgentSettingsRollingBack || op.Recovery == nil || op.Recovery.Owner != expected.Owner || op.Recovery.Revision != expected.Revision || op.Recovery.Settings != expected.Settings {
				return ports.ErrAgentSettingsConflict
			}
			expected = *op.Recovery
		} else {
			expected.ClaimedAt, expected.PreviousCheckpoint = at, op.Checkpoint
		}
		raw, err := json.Marshal(settingsRecoveryRecord{AgentSettingsRecovery: expected, Plan: expected.Plan, Qualification: expected.Qualification})
		if err != nil {
			return err
		}
		category := op.ErrorCategory
		if category == "" {
			category = domain.AgentSettingsErrorRecovery
		}
		changed, err := q.AdvanceAgentSettingsRecovery(ctx, gen.AdvanceAgentSettingsRecoveryParams{SessionID: string(id), ID: operationID, ExpectedVersion: expectedVersion, Checkpoint: string(checkpoint), RecoveryJson: string(raw), ErrorCategory: string(category), At: at})
		if err != nil {
			return err
		}
		if changed != 1 {
			return ports.ErrAgentSettingsConflict
		}
		if op.Phase != domain.AgentSettingsRecoveryRequired {
			if err := q.InsertAgentSettingsAudit(ctx, gen.InsertAgentSettingsAuditParams{OperationID: op.ID, Phase: string(domain.AgentSettingsRecoveryRequired), RecordedAt: at, ErrorCategory: string(category), RequestID: op.RequestID}); err != nil {
				return err
			}
		}
		op.Phase, op.Checkpoint, op.ErrorCategory, op.Recovery, op.UpdatedAt = domain.AgentSettingsRecoveryRequired, checkpoint, category, &expected, at
		op.Version++
		result = op
		return nil
	})
	return result, err
}

// Provider wire evidence is persisted privately, never serialized in API GETs.
type settingsRecoveryRecord struct {
	domain.AgentSettingsRecovery
	Plan          *domain.AgentSettingsProviderPlan  `json:"plan,omitempty"`
	Qualification *domain.AgentSettingsQualification `json:"qualification,omitempty"`
}

func decodeSettingsRecovery(raw string) (*domain.AgentSettingsRecovery, error) {
	var record settingsRecoveryRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return nil, err
	}
	record.AgentSettingsRecovery.Plan = record.Plan
	record.AgentSettingsRecovery.Qualification = record.Qualification
	return &record.AgentSettingsRecovery, nil
}
