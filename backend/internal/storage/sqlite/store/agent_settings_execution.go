package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// PinAgentSettingsExecution saves a qualified non-secret preimage before the
// first provider action. Only the generation may differ from the original
// request, and only a complete current Chat owner can execute or recover it.
func (s *Store) PinAgentSettingsExecution(ctx context.Context, id domain.SessionID, operationID string, expectedVersion int64, execution domain.AgentSettingsExecution, at time.Time) (domain.AgentSettingsOperation, error) {
	if at.IsZero() || expectedVersion < 0 || execution.Revision < 0 || execution.Plan.Source.Validate() != nil || execution.Plan.Target.Validate() != nil || execution.Qualification.InputFingerprint == "" || execution.Qualification.PolicyVersion == "" {
		return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsInvalid
	}
	for _, value := range []string{execution.Plan.Protocol, execution.Plan.ProviderConversationID, execution.Qualification.InputFingerprint, execution.Qualification.PolicyVersion, execution.Plan.Before.Model, execution.Plan.Before.Effort, execution.Plan.Before.Fast, execution.Plan.After.Model, execution.Plan.After.Effort, execution.Plan.After.Fast, execution.Plan.Permission} {
		if len(value) > 512 || !utf8.ValidString(value) || strings.ContainsFunc(value, unicode.IsControl) {
			return domain.AgentSettingsOperation{}, ports.ErrAgentSettingsInvalid
		}
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return domain.AgentSettingsOperation{}, err
	}
	defer s.writeMu.Unlock()
	var result domain.AgentSettingsOperation
	err := s.inTx(ctx, "pin settings execution", func(q *gen.Queries) error {
		row, err := q.GetAgentSettingsOperation(ctx, gen.GetAgentSettingsOperationParams{SessionID: string(id), ID: operationID})
		if err != nil {
			return settingsStoreError(err)
		}
		op, err := decodeSettingsOperation(row)
		if err != nil {
			return err
		}
		if op.Phase != domain.AgentSettingsPending || op.Checkpoint != domain.AgentSettingsQueued || op.StartedAt != nil || op.Version != expectedVersion {
			return ports.ErrAgentSettingsConflict
		}
		current, err := q.GetSessionAgentSettings(ctx, id)
		if err != nil {
			return settingsStoreError(err)
		}
		if execution.Owner.AccountID != op.AccountID || execution.Owner.ProviderConversationID != op.ProviderConversationID || execution.Owner.BranchID != op.BranchID || execution.Revision != op.BaseRevision || !settingsRecoveryMatches(current, domain.AgentSettingsRecovery{Owner: execution.Owner, Revision: execution.Revision, Settings: op.Source}) {
			return ports.ErrAgentSettingsConflict
		}
		if execution.Plan.Protocol == "" || execution.Plan.Before.Model == "" || execution.Plan.ProviderConversationID != execution.Owner.ProviderConversationID || execution.Plan.Source != op.Source || execution.Plan.Target != op.Target {
			return fmt.Errorf("%w: execution_preimage_invalid", ports.ErrAgentSettingsInvalid)
		}
		raw, err := json.Marshal(execution)
		if err != nil {
			return err
		}
		changed, err := q.PinAgentSettingsExecution(ctx, gen.PinAgentSettingsExecutionParams{SessionID: string(id), ID: operationID, ExpectedVersion: expectedVersion, ExecutionJson: string(raw), At: at})
		if err != nil {
			return err
		}
		if changed != 1 {
			return ports.ErrAgentSettingsConflict
		}
		op.Execution = &execution
		op.Version++
		op.UpdatedAt = at
		result = op
		return nil
	})
	return result, err
}
