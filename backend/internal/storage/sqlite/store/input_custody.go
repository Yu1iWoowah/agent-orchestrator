package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

var _ ports.InputCustodyStore = (*Store)(nil)

// RegisterInputCustody records provenance without changing dispatch state.
// No lifecycle currently calls these shared storage operations. PR1's owner
// coordinator and 147's delivery coordinator must supply their own admission,
// delivery evidence and lifecycle barrier before changing custody or holds.
func (s *Store) RegisterInputCustody(ctx context.Context, turnID, owner, actionKey string, at time.Time) (domain.InputCustodyReceipt, error) {
	return s.applyInputCustodyAction(ctx, gen.InsertInputCustodyActionParams{
		ActionKey: actionKey, TurnID: turnID, Kind: "register", TargetOwner: owner, CreatedAt: at,
	})
}

// HoldConversationInput records a hold and removes the input from dispatch in
// the same transaction, preserving its payload and provider evidence.
func (s *Store) HoldConversationInput(ctx context.Context, expected domain.InputCustodyVersion, hold domain.InputHoldVersion, reason, actionKey string, at time.Time) (domain.InputCustodyReceipt, error) {
	return s.applyInputCustodyAction(ctx, gen.InsertInputCustodyActionParams{
		ActionKey: actionKey, TurnID: expected.TurnID, Kind: "hold", ExpectedOwner: expected.Owner, ExpectedRevision: expected.Revision,
		HoldKey: hold.Key, HoldOwner: hold.Owner, HoldVersion: hold.Version, Reason: reason, CreatedAt: at,
	})
}

// ReleaseConversationInput consumes the hold producer's authorization. It does
// not infer known-not-sent or permission to replay from a storage receipt. Other
// active holds continue to fence the input even after this layer is released.
func (s *Store) ReleaseConversationInput(ctx context.Context, expected domain.InputCustodyVersion, hold domain.InputHoldVersion, state domain.TurnState, actionKey string, at time.Time) (domain.InputCustodyReceipt, error) {
	return s.applyInputCustodyAction(ctx, gen.InsertInputCustodyActionParams{
		ActionKey: actionKey, TurnID: expected.TurnID, Kind: "release", ExpectedOwner: expected.Owner, ExpectedRevision: expected.Revision,
		HoldKey: hold.Key, HoldOwner: hold.Owner, HoldVersion: hold.Version, TargetState: string(state), CreatedAt: at,
	})
}

// TransferInputCustody changes only routing. Hold reasons, ledger owners and
// versions, original queue position, payload and provider evidence stay intact.
func (s *Store) TransferInputCustody(ctx context.Context, expected domain.InputCustodyVersion, owner, actionKey string, at time.Time) (domain.InputCustodyReceipt, error) {
	return s.applyInputCustodyAction(ctx, gen.InsertInputCustodyActionParams{
		ActionKey: actionKey, TurnID: expected.TurnID, Kind: "transfer", ExpectedOwner: expected.Owner, ExpectedRevision: expected.Revision,
		TargetOwner: owner, CreatedAt: at,
	})
}

// DeleteConversationInput is the explicit retention path. General orphan
// cleanup and session cascades cannot impersonate a custody-qualified delete.
func (s *Store) DeleteConversationInput(ctx context.Context, expected domain.InputCustodyVersion, actionKey string, at time.Time) (domain.InputCustodyReceipt, error) {
	return s.applyInputCustodyAction(ctx, gen.InsertInputCustodyActionParams{
		ActionKey: actionKey, TurnID: expected.TurnID, Kind: "delete", ExpectedOwner: expected.Owner, ExpectedRevision: expected.Revision, CreatedAt: at,
	})
}

func (s *Store) applyInputCustodyAction(ctx context.Context, action gen.InsertInputCustodyActionParams) (domain.InputCustodyReceipt, error) {
	var receipt domain.InputCustodyReceipt
	if action.ActionKey == "" || action.TurnID == "" || action.CreatedAt.IsZero() {
		return receipt, domain.ErrInputCustodyConflict
	}
	_, unlock := s.conversationWriter(ctx)
	defer unlock()
	err := s.inTx(ctx, "input custody action", func(q *gen.Queries) error {
		if err := inputCustodyStorageCompatibilityGate(ctx, q, s.readDB); err != nil {
			return err
		}
		prior, err := q.SelectInputCustodyAction(ctx, action.ActionKey)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if prior.TurnID != action.TurnID || prior.Kind != action.Kind || prior.ExpectedOwner != action.ExpectedOwner ||
				prior.ExpectedRevision != action.ExpectedRevision || prior.TargetOwner != action.TargetOwner || prior.HoldKey != action.HoldKey ||
				prior.HoldOwner != action.HoldOwner || prior.HoldVersion != action.HoldVersion || prior.Reason != action.Reason || prior.TargetState != action.TargetState {
				return domain.ErrInputCustodyConflict
			}
		} else if err := q.InsertInputCustodyAction(ctx, action); err != nil {
			if strings.Contains(err.Error(), "INPUT_") {
				return errors.Join(domain.ErrInputCustodyConflict, err)
			}
			return err
		}
		receipt = domain.InputCustodyReceipt{ActionKey: action.ActionKey, TurnID: action.TurnID, Revision: action.ExpectedRevision + 1}
		return nil
	})
	return receipt, err
}

// InputCustodyStorageCompatibility is the shared-schema preflight. It never
// qualifies a provider or host, and it does not authorize releasing a hold.
func (s *Store) InputCustodyStorageCompatibility(ctx context.Context) error {
	return inputCustodyStorageCompatibilityGate(ctx, s.conversationReader(ctx), s.readDB)
}

// SQLite's catalog and pragma table functions are not application tables known
// to sqlc. Inspect the committed schema through the reader pool; check the
// action transaction's hold facts separately through its transaction-bound q.
const inputCustodySchemaReadySQL = `SELECT CAST(
 (SELECT foreign_keys FROM pragma_foreign_keys) = 1
 AND NOT EXISTS(SELECT 1 FROM pragma_foreign_key_check)
 AND (SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name IN (
  'conversation_input_action_validate',
  'conversation_input_action_no_update',
  'conversation_input_action_no_delete',
  'conversation_input_custody_insert',
  'conversation_input_custody_update',
  'conversation_input_custody_delete',
  'conversation_input_hold_insert',
  'conversation_input_hold_update',
  'conversation_input_hold_delete',
  'conversation_input_turn_insert',
  'conversation_input_turn_update',
  'conversation_input_turn_delete',
  'conversation_input_message_insert',
  'conversation_input_message_update',
  'conversation_input_message_delete',
  'conversation_input_activity_update',
  'conversation_input_activity_delete',
  'conversation_input_activity_insert',
  'conversation_input_action_apply',
  'conversation_input_action_cdc'
 )) = 20
 AND EXISTS(SELECT 1 FROM sqlite_master WHERE type='view' AND name='orchestrator_replacement_held_turns' AND instr(sql,'conversation_input_holds') > 0)
 AND EXISTS(SELECT 1 FROM sqlite_master WHERE type='trigger' AND name='replacement_turn_dispatch_fence') AS INTEGER)`

func inputCustodyStorageCompatibilityGate(ctx context.Context, q *gen.Queries, schema gen.DBTX) error {
	var ready int
	if err := schema.QueryRowContext(ctx, inputCustodySchemaReadySQL).Scan(&ready); err != nil {
		return fmt.Errorf("input custody schema compatibility: %w", err)
	}
	consistent, err := q.InputCustodyStateConsistent(ctx)
	if err != nil {
		return fmt.Errorf("input custody state compatibility: %w", err)
	}
	if ready != 1 || consistent != 1 {
		return fmt.Errorf("input custody storage compatibility: %w", domain.ErrInputCustodyConflict)
	}
	return nil
}

// ConversationInputCustody returns provenance and holds in canonical input order.
func (s *Store) ConversationInputCustody(ctx context.Context, conversationID string) ([]domain.InputCustody, error) {
	// Read custody and its holds from the same snapshot during concurrent CAS.
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("read input custody: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := gen.New(tx)
	rows, err := q.ListConversationInputCustody(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	result := make([]domain.InputCustody, 0, len(rows))
	for _, row := range rows {
		holds, err := q.SelectInputHolds(ctx, row.TurnID)
		if err != nil {
			return nil, err
		}
		item := domain.InputCustody{
			InputCustodyVersion: domain.InputCustodyVersion{TurnID: row.TurnID, Owner: row.Owner, Revision: row.Revision},
			OriginalState:       domain.TurnState(row.OriginalState), OriginalConversationID: row.OriginalConversationID,
			OriginalBranchID: row.OriginalBranchID, OriginalSessionID: domain.SessionID(row.OriginalSessionID),
			OriginalGeneration: row.OriginalGeneration, OriginalSequence: row.OriginalSequence, RequestedAt: row.RequestedAt,
			Holds: make([]domain.InputHold, 0, len(holds)),
		}
		for _, hold := range holds {
			item.Holds = append(item.Holds, domain.InputHold{
				InputHoldVersion: domain.InputHoldVersion{Key: hold.HoldKey, Owner: hold.LedgerOwner, Version: hold.Version},
				Reason:           hold.Reason, Active: hold.Active != 0,
			})
		}
		result = append(result, item)
	}
	return result, tx.Commit()
}
