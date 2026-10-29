package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// The physical handoff and settings intention are claimed by the same writer
// transaction. No public caller can manufacture the launch fence in op.TUI.
func prepareTUISettingsHandoff(ctx context.Context, q *gen.Queries, op *domain.AgentSettingsOperation) error {
	row, err := q.GetSession(ctx, op.SessionID)
	if err != nil {
		return settingsStoreError(err)
	}
	if row.SessionMode != domain.SessionModeTUI {
		return ports.ErrAgentSettingsInvalid
	}
	if row.RuntimeHandleID == "" && row.RuntimeLaunchID == "" && row.AgentSessionID == "" {
		return nil
	}
	if row.RuntimeHandleID == "" || row.RuntimeLaunchID == "" || row.AgentSessionID == "" || row.AgentSessionIDLaunchID != row.RuntimeLaunchID || op.AccountID == "" {
		return ports.ErrAgentSettingsConflict
	}
	op.TUI = &domain.AgentSettingsTUIHandoff{SourceRuntimeHandle: row.RuntimeHandleID, TargetGeneration: op.ID + ":target"}
	return nil
}

func insertTUISettingsHandoff(ctx context.Context, q *gen.Queries, op domain.AgentSettingsOperation) error {
	policy := domain.SessionInterfaceTransitionDrain
	if op.StopTurn {
		policy = domain.SessionInterfaceTransitionInterrupt
	}
	_, err := q.InsertSessionInterfaceTransition(ctx, gen.InsertSessionInterfaceTransitionParams{
		ID: op.ID, SessionID: op.SessionID, SettingsOperationID: op.ID,
		SourceMode: domain.SessionModeTUI, TargetMode: domain.SessionModeTUI,
		Policy: policy, HistoryPolicy: domain.SessionInterfaceTransitionHistoryStrict,
		Phase: domain.SessionInterfaceTransitionRequested, NativeConversationID: op.ProviderConversationID,
		CreatedAt: op.CreatedAt, UpdatedAt: op.CreatedAt,
	})
	if isSQLiteUnique(err) {
		return ports.ErrAgentSettingsConflict
	}
	return err
}

// Unlinked TUI operations remain unable to start. A linked restart has durable
// source/target process identities and an exclusive physical handoff journal.
func settingsOperationCanRecover(row gen.GetSessionAgentSettingsRow, op domain.AgentSettingsOperation) bool {
	if op.TUI == nil {
		return settingsOwnerCanRecover(row)
	}
	return !row.IsTerminated && row.SessionMode == domain.SessionModeTUI && row.RuntimeLaunchID != "" &&
		row.AccountID != "" && row.AccountID == op.AccountID && row.ProviderConversationID != "" && row.ProviderConversationID == op.ProviderConversationID &&
		op.ApplyMode == domain.AgentSettingsRestart && op.TUI.SourceRuntimeHandle != "" && op.TUI.TargetGeneration != ""
}

func settingsOperationRecoveryMatches(row gen.GetSessionAgentSettingsRow, op domain.AgentSettingsOperation, recovery domain.AgentSettingsRecovery) bool {
	if op.TUI == nil {
		return settingsRecoveryMatches(row, recovery)
	}
	owner := domain.AgentSettingsOwner{SessionID: row.ID, AccountID: row.AccountID, ControllerGeneration: row.RuntimeLaunchID, ProviderConversationID: row.ProviderConversationID, BranchID: row.BranchID}
	settings := domain.AgentSettings{Model: domain.AgentModelSelection(row.Model), Effort: domain.AgentEffortSelection(row.Effort), FastMode: domain.AgentFastMode(row.AgentFastMode)}
	return settingsOperationCanRecover(row, op) && recovery.Owner == owner && recovery.Revision == row.AgentSettingsRevision && recovery.Settings == settings
}

func settleTUISettingsHandoff(ctx context.Context, q *gen.Queries, op domain.AgentSettingsOperation, phase domain.AgentSettingsPhase, at time.Time) error {
	tr, err := q.GetSessionInterfaceTransition(ctx, op.ID)
	if err != nil {
		return settingsStoreError(err)
	}
	if tr.SettingsOperationID != op.ID || tr.SessionID != op.SessionID || !interfaceTransitionToDomain(tr).Active() {
		return ports.ErrAgentSettingsConflict
	}
	next := domain.SessionInterfaceTransitionPhase(phase)
	switch phase {
	case domain.AgentSettingsApplied:
		next = domain.SessionInterfaceTransitionCompleted
	case domain.AgentSettingsCancelled:
		if tr.Phase != domain.SessionInterfaceTransitionRequested && tr.Phase != domain.SessionInterfaceTransitionPreflighting && tr.Phase != domain.SessionInterfaceTransitionDraining {
			return ports.ErrAgentSettingsConflict
		}
	case domain.AgentSettingsFailed, domain.AgentSettingsRecoveryRequired:
	default:
		return ports.ErrAgentSettingsInvalid
	}
	completed := sql.NullTime{Time: at, Valid: phase != domain.AgentSettingsRecoveryRequired}
	changed, err := q.AdvanceSessionInterfaceTransition(ctx, gen.AdvanceSessionInterfaceTransitionParams{ID: tr.ID, Phase: next, Phase_2: tr.Phase, NativeConversationID: tr.NativeConversationID, ErrorCode: op.ErrorCode, UpdatedAt: at, CompletedAt: completed})
	if err != nil {
		return err
	}
	if changed != 1 {
		return ports.ErrAgentSettingsConflict
	}
	return nil
}

// RecordTUIAgentSettingsRuntime pins a returned native launch handle before
// start verification, so a replacement daemon can stop it before restoring.
func (s *Store) RecordTUIAgentSettingsRuntime(ctx context.Context, id domain.SessionID, operationID, targetGeneration, handle string, rollback bool) error {
	if handle == "" || targetGeneration == "" {
		return ports.ErrAgentSettingsInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	slot := "$.tui.targetRuntimeHandle"
	if rollback {
		slot = "$.tui.rollbackRuntimeHandle"
	}
	changed, err := s.qw.RecordTUISettingsRuntime(ctx, gen.RecordTUISettingsRuntimeParams{SessionID: string(id), ID: operationID, TargetGeneration: targetGeneration, Slot: slot, Handle: handle})
	if err != nil {
		return err
	}
	if changed != 1 {
		return ports.ErrAgentSettingsConflict
	}
	return nil
}

// SetTUIAgentSettingsRuntimeUnknown fences a launch before Create. Only a
// returned handle or a proven no-effect outcome can clear the obligation.
func (s *Store) SetTUIAgentSettingsRuntimeUnknown(ctx context.Context, id domain.SessionID, operationID string, unknown bool) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	value := "false"
	if unknown {
		value = "true"
	}
	n, err := s.qw.SetTUISettingsRuntimeUnknown(ctx, gen.SetTUISettingsRuntimeUnknownParams{SessionID: string(id), ID: operationID, Value: value})
	if err != nil {
		return err
	}
	if n != 1 {
		return ports.ErrAgentSettingsConflict
	}
	return nil
}
