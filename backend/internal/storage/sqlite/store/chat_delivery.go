package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func chatJournalOwner(row gen.GetSessionAgentSettingsRow) domain.AgentSettingsOwner {
	return domain.AgentSettingsOwner{SessionID: row.ID, AccountID: row.AccountID, ControllerGeneration: row.ControllerGeneration, ProviderConversationID: row.ProviderConversationID, BranchID: row.BranchID}
}

func decodeChatDelivery(row gen.ChatDeliveryAttempt) (domain.ChatDeliveryAttempt, error) {
	var a domain.ChatDeliveryAttempt
	if err := json.Unmarshal([]byte(row.RecordJson), &a); err != nil {
		return a, err
	}
	if a.ID != row.ID || string(a.SessionID) != row.SessionID || a.ConversationID != row.ConversationID || a.Owner.ControllerGeneration != row.OwnerGeneration || a.Version != row.Version || string(a.Disposition) != row.Disposition || int64(a.EvidenceLevel) != row.EvidenceLevel {
		return a, ports.ErrAgentSettingsConflict
	}
	return a, a.Validate()
}

// ReserveChatDelivery durably reserves every input before crossing provider I/O.
// An unresolved or accepted prior attempt is never made replayable by cleanup.
func (s *Store) ReserveChatDelivery(ctx context.Context, a domain.ChatDeliveryAttempt) (domain.ChatDeliveryAttempt, bool, error) {
	if a.Validate() != nil || a.Version != 0 || a.Disposition != domain.ChatDeliveryReserved || a.Receipt != nil || a.EvidenceLevel != 0 || a.ExecutionSettled {
		return a, false, ports.ErrAgentSettingsInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return a, false, err
	}
	defer s.writeMu.Unlock()
	created := false
	err := s.inTx(ctx, "reserve chat delivery", func(q *gen.Queries) error {
		current, err := q.GetSessionAgentSettings(ctx, a.SessionID)
		if err != nil {
			return err
		}
		if current.IsTerminated || chatJournalOwner(current) != a.Owner || current.AgentSettingsRevision != a.Revision {
			return ports.ErrAgentSettingsConflict
		}
		row, lookupErr := q.GetChatDeliveryAttempt(ctx, gen.GetChatDeliveryAttemptParams{SessionID: string(a.SessionID), ID: a.ID})
		if lookupErr == nil {
			original, err := decodeChatDelivery(row)
			if err != nil {
				return err
			}
			if original.Owner != a.Owner || original.Revision != a.Revision || original.ConversationID != a.ConversationID || !slices.Equal(original.TurnIDs, a.TurnIDs) {
				return ports.ErrAgentSettingsConflict
			}
			a = original
			return nil
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return lookupErr
		}
		a.InputRefs = make([]domain.ChatDeliveryInputRef, 0, len(a.TurnIDs))
		for _, id := range a.TurnIDs {
			turn, err := q.SelectConversationTurnByID(ctx, id)
			if err != nil {
				return err
			}
			if turn.ConversationID != a.ConversationID || (turn.State != "queued" && turn.State != "running") {
				return ports.ErrAgentSettingsConflict
			}
			if turn.BranchID != a.Owner.BranchID || turn.HandledBySessionID != a.SessionID {
				var snapshot domain.AgentSettingsTurnSnapshot
				if json.Unmarshal([]byte(turn.AgentSettingsSnapshotJson), &snapshot) != nil || snapshot.Owner == nil || *snapshot.Owner != a.Owner || snapshot.Revision == nil || *snapshot.Revision != a.Revision {
					return ports.ErrAgentSettingsConflict
				}
			}
			a.InputRefs = append(a.InputRefs, domain.ChatDeliveryInputRef{TurnID: id, BranchID: turn.BranchID, SessionID: turn.HandledBySessionID})
			blocked, err := q.HasUnresolvedChatDelivery(ctx, id)
			if err != nil {
				return err
			}
			if blocked {
				return ports.ErrAgentSettingsConflict
			}
		}
		raw, err := json.Marshal(a)
		if err != nil {
			return err
		}
		if err := q.InsertChatDeliveryAttempt(ctx, gen.InsertChatDeliveryAttemptParams{ID: a.ID, SessionID: string(a.SessionID), ConversationID: a.ConversationID, OwnerGeneration: a.Owner.ControllerGeneration, Disposition: string(a.Disposition), EvidenceLevel: 0, Version: 0, RecordJson: string(raw), CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}); err != nil {
			return err
		}
		for i, id := range a.TurnIDs {
			if err := q.InsertChatDeliveryMember(ctx, gen.InsertChatDeliveryMemberParams{AttemptID: a.ID, TurnID: id, Ordinal: int64(i)}); err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	return a, created && err == nil, err
}

// UpdateChatDelivery records a correlated receipt or blocked metadata using
// owner and version CAS. Reattachment may adopt facts, never a new execution.
func (s *Store) UpdateChatDelivery(ctx context.Context, next domain.ChatDeliveryAttempt, expected int64, actor domain.AgentSettingsOwner) (domain.ChatDeliveryAttempt, error) {
	if next.Validate() != nil || expected < 0 || next.Version != expected+1 {
		return next, ports.ErrAgentSettingsInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return next, err
	}
	defer s.writeMu.Unlock()
	err := s.inTx(ctx, "update chat delivery", func(q *gen.Queries) error {
		row, err := q.GetChatDeliveryAttempt(ctx, gen.GetChatDeliveryAttemptParams{SessionID: string(next.SessionID), ID: next.ID})
		if err != nil {
			return err
		}
		old, err := decodeChatDelivery(row)
		if err != nil {
			return err
		}
		current, err := q.GetSessionAgentSettings(ctx, next.SessionID)
		if err != nil {
			return err
		}
		if current.IsTerminated || chatJournalOwner(current) != actor || actor.AccountID != old.Owner.AccountID || actor.ProviderConversationID != old.Owner.ProviderConversationID || actor.BranchID != old.Owner.BranchID || old.Version != expected || old.Owner != next.Owner || old.Revision != next.Revision || old.ConversationID != next.ConversationID || !slices.Equal(old.TurnIDs, next.TurnIDs) || !slices.Equal(old.InputRefs, next.InputRefs) || !old.CreatedAt.Equal(next.CreatedAt) || next.UpdatedAt.Before(old.UpdatedAt) || next.LastProgressAt.Before(old.LastProgressAt) || old.PreparedTurnID != "" && next.PreparedTurnID != old.PreparedTurnID || old.ExecutionSettled && !next.ExecutionSettled {
			return ports.ErrAgentSettingsConflict
		}
		if !old.ExecutionSettled && next.ExecutionSettled && (next.Receipt == nil || !next.Receipt.ExecutionSettled) {
			return ports.ErrAgentSettingsInvalid
		}
		if next.Receipt != nil {
			if !old.Accepts(*next.Receipt) || next.Disposition != next.Receipt.Disposition {
				return ports.ErrAgentSettingsConflict
			}
		} else if old.Receipt != nil || next.Disposition != old.Disposition {
			return ports.ErrAgentSettingsConflict
		}
		level := 0
		if next.Disposition == domain.ChatDeliveryAccepted {
			level = 2
		}
		if next.EvidenceLevel != level {
			return ports.ErrAgentSettingsInvalid
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		n, err := q.UpdateChatDeliveryAttempt(ctx, gen.UpdateChatDeliveryAttemptParams{Disposition: string(next.Disposition), EvidenceLevel: int64(level), RecordJson: string(raw), UpdatedAt: next.UpdatedAt, SessionID: string(next.SessionID), ID: next.ID, ExpectedVersion: expected})
		if err != nil {
			return err
		}
		if n != 1 {
			return ports.ErrAgentSettingsConflict
		}
		// Progress recovery closes this episode atomically. A later silence can
		// create a fresh question instead of leaving a dismissed warning invisible.
		attentionID := old.AttentionID
		if attentionID == "" {
			attentionID = next.AttentionID
		}
		recovered := old.BlockedSince != nil && next.BlockedSince == nil
		if attentionID != "" && (recovered || next.ExecutionSettled || next.Disposition == domain.ChatDeliveryNotSent) {
			question, err := questionRow(ctx, q, next.SessionID, attentionID)
			if err != nil {
				return err
			}
			if question.ClientKey != "delivery-"+next.ID && !strings.HasPrefix(question.ClientKey, "delivery-"+next.ID+"-") {
				return ports.ErrAgentSettingsConflict
			}
			if question.State == "open" {
				reason := "原執行已確認結束"
				if next.Disposition == domain.ChatDeliveryNotSent {
					reason = "原輸入已確認未送出"
				} else if !next.ExecutionSettled {
					reason = "原執行已恢復進度"
				}
				n, err := q.CloseAttentionQuestion(ctx, gen.CloseAttentionQuestionParams{ID: question.ID, OriginSessionID: string(next.SessionID), ExpectedVersion: question.Version, State: "cancelled", Actor: "agent", Reason: reason, At: sql.NullTime{Time: next.UpdatedAt.UTC(), Valid: true}})
				if err != nil {
					return err
				}
				if n != 1 {
					return ports.ErrAgentSettingsConflict
				}
				row, err := questionRow(ctx, q, next.SessionID, attentionID)
				if err != nil {
					return err
				}
				record, err := questionRecord(ctx, q, row)
				if err != nil {
					return err
				}
				if err := appendQuestionCompatibility(ctx, q, record, "cancelled", reason, next.UpdatedAt); err != nil {
					return err
				}
			}
		}
		if next.Disposition == domain.ChatDeliveryNotSent && old.Disposition != domain.ChatDeliveryNotSent {
			for _, input := range next.InputRefs {
				if _, err := q.RequeueUnforwardedDeliveryMember(ctx, gen.RequeueUnforwardedDeliveryMemberParams{
					TurnID: input.TurnID, ConversationID: next.ConversationID, BranchID: input.BranchID,
					SessionID: input.SessionID, PreparedTurnID: next.PreparedTurnID,
				}); err != nil {
					return err
				}
			}
		}
		if next.Receipt != nil {
			proof, err := json.Marshal(next.Receipt)
			if err != nil {
				return err
			}
			return q.InsertChatDeliveryReceipt(ctx, gen.InsertChatDeliveryReceiptParams{AttemptID: next.ID, Version: next.Version, ReceiptJson: string(proof), RecordedAt: next.UpdatedAt})
		}
		return nil
	})
	return next, err
}

// ChatDeliveryAttempt reads a stable lookup key after a lost acknowledgement.
func (s *Store) ChatDeliveryAttempt(ctx context.Context, id domain.SessionID, key string) (domain.ChatDeliveryAttempt, error) {
	row, err := s.qr.GetChatDeliveryAttempt(ctx, gen.GetChatDeliveryAttemptParams{SessionID: string(id), ID: key})
	if err != nil {
		return domain.ChatDeliveryAttempt{}, err
	}
	return decodeChatDelivery(row)
}

// ChatDeliveryAttemptForTurn reads the original execution rather than replaying it.
func (s *Store) ChatDeliveryAttemptForTurn(ctx context.Context, id domain.SessionID, turn string) (domain.ChatDeliveryAttempt, error) {
	row, err := s.qr.GetChatDeliveryAttemptForTurn(ctx, gen.GetChatDeliveryAttemptForTurnParams{SessionID: string(id), TurnID: turn})
	if err != nil {
		return domain.ChatDeliveryAttempt{}, err
	}
	return decodeChatDelivery(row)
}

// PendingChatDeliveries includes accepted work whose execution is not settled.
func (s *Store) PendingChatDeliveries(ctx context.Context, id domain.SessionID) ([]domain.ChatDeliveryAttempt, error) {
	rows, err := s.qr.ListPendingChatDeliveryAttempts(ctx, string(id))
	if err != nil {
		return nil, err
	}
	result := make([]domain.ChatDeliveryAttempt, 0, len(rows))
	for _, row := range rows {
		a, err := decodeChatDelivery(row)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, nil
}
