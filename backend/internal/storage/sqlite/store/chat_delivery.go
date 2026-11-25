package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

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
		return a, &ports.ChatDeliveryDecodeError{Cause: err}
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
		return updateChatDelivery(ctx, q, next, expected, &actor, false)
	})
	return next, err
}

// BlockChatDeliveryWithAttention creates the delivery prompt and links it to
// its attempt in one transaction, so a prompt is never visible unlinked. A
// failed owner/version check rolls both back; the next block retries.
func (s *Store) BlockChatDeliveryWithAttention(ctx context.Context, next domain.ChatDeliveryAttempt, expected int64, actor domain.AgentSettingsOwner, prompt domain.AttentionQuestionCreate, at time.Time) (domain.ChatDeliveryAttempt, error) {
	if next.AttentionID != "" || next.BlockedSince == nil || prompt.OriginSessionID != next.SessionID || next.Validate() != nil || expected < 0 || next.Version != expected+1 {
		return next, ports.ErrAgentSettingsInvalid
	}
	prepared, err := prepareAttentionQuestion(prompt)
	if err != nil {
		return next, err
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return next, err
	}
	defer s.writeMu.Unlock()
	err = s.inTx(ctx, "block chat delivery", func(q *gen.Queries) error {
		question, _, err := insertAttentionQuestion(ctx, q, prepared, at)
		if err != nil {
			return err
		}
		next.AttentionID = question.ID
		return updateChatDelivery(ctx, q, next, expected, &actor, false)
	})
	if err != nil {
		next.AttentionID = ""
	}
	return next, err
}

// SettleInterruptedUnknownDelivery closes the original inputs without making
// them replayable. The caller has proved that their execution scope ended.
func (s *Store) SettleInterruptedUnknownDelivery(ctx context.Context, original domain.ChatDeliveryAttempt, at time.Time) error {
	if original.ExecutionSettled || original.Disposition != domain.ChatDeliveryUnknown && original.Disposition != domain.ChatDeliveryReserved {
		return ports.ErrAgentSettingsInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "settle interrupted unknown delivery", func(q *gen.Queries) error {
		code := "CHAT_DELIVERY_HOST_GONE_SETTLED"
		for _, id := range original.TurnIDs {
			turn, err := q.SelectConversationTurnByID(ctx, id)
			if err != nil {
				return err
			}
			if turn.ConversationID != original.ConversationID {
				return ports.ErrAgentSettingsConflict
			}
			if turn.State != domain.TurnStateCompleted {
				code = "CHAT_DELIVERY_HOST_GONE_INTERRUPTED"
			}
			if turn.State == domain.TurnStateQueued || turn.State == domain.TurnStateRunning {
				if err := settleLostChatTurn(ctx, q, turn, at); err != nil {
					return err
				}
			}
		}
		receipt := domain.ChatDeliveryReceipt{AttemptID: original.ID, Disposition: domain.ChatDeliveryUnknown, Source: "incomplete", ExecutionSettled: true, ObservedAt: at}
		if old := original.Receipt; old != nil {
			receipt.ProviderEpoch, receipt.ProviderRequestID, receipt.ProviderTurnID = old.ProviderEpoch, old.ProviderRequestID, old.ProviderTurnID
		}
		next := original
		next.Version++
		next.UpdatedAt = at
		next.Disposition, next.Receipt, next.ExecutionSettled, next.EvidenceLevel = domain.ChatDeliveryUnknown, &receipt, true, 0
		next.NextCheckAt, next.BlockedSince, next.ErrorCode = nil, nil, code
		return updateChatDelivery(ctx, q, next, original.Version, nil, false)
	})
}

// SettlePreviousBootAcceptance records that execution ended without rewriting
// what the provider originally acknowledged. The caller has verified the end
// of the original execution scope.
func (s *Store) SettlePreviousBootAcceptance(ctx context.Context, original domain.ChatDeliveryAttempt, actor domain.AgentSettingsOwner, at time.Time) error {
	if original.ExecutionSettled || original.Disposition != domain.ChatDeliveryAccepted || original.Receipt == nil ||
		original.Receipt.ExecutionSettled || original.Receipt.ProviderTurnID == "" || !original.Accepts(*original.Receipt) ||
		len(original.InputRefs) != len(original.TurnIDs) || len(original.InputRefs) == 0 {
		return ports.ErrAgentSettingsInvalid
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "settle previous boot acceptance", func(q *gen.Queries) error {
		code := "CHAT_DELIVERY_HOST_GONE_SETTLED"
		for i, ref := range original.InputRefs {
			turn, err := q.SelectConversationTurnByID(ctx, ref.TurnID)
			if err != nil {
				return err
			}
			if ref.TurnID != original.TurnIDs[i] || turn.ConversationID != original.ConversationID || turn.BranchID != ref.BranchID || turn.HandledBySessionID != ref.SessionID || turn.RolledBackAt.Valid || turn.HandledByReviewID.Valid {
				return ports.ErrAgentSettingsConflict
			}
			if i > 0 {
				if turn.State == domain.TurnStateRunning || turn.ProviderTurnID != "" {
					return ports.ErrAgentSettingsConflict
				}
				if turn.State == domain.TurnStateQueued {
					// The receipt covers the entire reserved batch. Link members
					// whose acceptance had not yet been projected before the reboot.
					n, err := q.CompleteQueuedConversationBatchMember(ctx, gen.CompleteQueuedConversationBatchMemberParams{
						CompletedAt: sql.NullTime{Time: at, Valid: true}, PromotedToTurnID: sql.NullString{String: original.TurnIDs[0], Valid: true},
						BatchDeliveryKind: batchDeliveryKind(""), ID: turn.ID, ConversationID: turn.ConversationID,
					})
					if err != nil {
						return err
					}
					if n != 1 {
						return ports.ErrAgentSettingsConflict
					}
				}
				continue
			}
			providerTurn := original.Receipt.ProviderTurnID
			running := turn.State == domain.TurnStateRunning
			reconnectFailed := turn.State == domain.TurnStateFailed && turn.CompletedAt.Valid && turn.ErrorMessage == reconnectUnconfirmedDelivery
			interrupted := turn.State == domain.TurnStateInterrupted
			needsBinding := turn.ProviderTurnID == "" && (running || reconnectFailed || interrupted)
			if turn.ProviderTurnID != providerTurn && !needsBinding {
				return ports.ErrAgentSettingsConflict
			}
			if turn.State != domain.TurnStateCompleted {
				code = "CHAT_DELIVERY_HOST_GONE_INTERRUPTED"
			}
			if running || needsBinding {
				if turn.ControllerGeneration != original.Owner.ControllerGeneration || turn.HandledBySessionID != original.SessionID || turn.BranchID != actor.BranchID {
					return ports.ErrAgentSettingsConflict
				}
			}
			if needsBinding {
				// Acceptance can precede the binding and survive live reconnect cleanup.
				if original.PreparedTurnID != "" && original.PreparedTurnID != providerTurn {
					return ports.ErrAgentSettingsConflict
				}
				projected, err := q.SelectConversationTurnByProviderID(ctx, gen.SelectConversationTurnByProviderIDParams{
					ConversationID: turn.ConversationID, ProviderTurnID: providerTurn,
				})
				switch {
				case errors.Is(err, sql.ErrNoRows):
					if err := q.BindConversationTurnProviderID(ctx, gen.BindConversationTurnProviderIDParams{
						ID: turn.ID, ProviderTurnID: providerTurn,
						StartedAt: sql.NullTime{Time: original.Receipt.ObservedAt, Valid: running},
					}); err != nil {
						return err
					}
				case err != nil:
					return err
				default:
					// TurnStarted can adopt the native ID under a later reconnect
					// generation. Its branch and session still identify the receipt's
					// provider thread. Keep the existing event-to-turn correlation.
					if projected.BranchID != turn.BranchID || projected.HandledBySessionID != turn.HandledBySessionID ||
						projected.HandledByReviewID.Valid || projected.RolledBackAt.Valid || projected.ControllerGeneration == "" ||
						(projected.State != domain.TurnStateRunning && !projected.State.Terminal()) {
						return ports.ErrAgentSettingsConflict
					}
					hasDelivery, err := q.HasUnresolvedChatDelivery(ctx, projected.ID)
					if err != nil {
						return err
					}
					if hasDelivery {
						return ports.ErrAgentSettingsConflict
					}
					if projected.State == domain.TurnStateRunning {
						if err := settleLostChatTurn(ctx, q, projected, at); err != nil {
							return err
						}
					}
				}
			}
			if running {
				if err := settleLostChatTurn(ctx, q, turn, at); err != nil {
					return err
				}
			}
		}
		next := original
		next.Version++
		next.UpdatedAt = at
		next.ExecutionSettled = true
		next.NextCheckAt, next.BlockedSince, next.ErrorCode = nil, nil, code
		return updateChatDelivery(ctx, q, next, original.Version, &actor, true)
	})
}

// Only interrupted unknown settlement has no actor. Its original execution
// has ended, and the session may already be owned by a different branch.
func updateChatDelivery(ctx context.Context, q *gen.Queries, next domain.ChatDeliveryAttempt, expected int64, actor *domain.AgentSettingsOwner, previousBoot bool) error {
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
	if actor != nil && (chatJournalOwner(current) != *actor || actor.AccountID != old.Owner.AccountID || actor.ProviderConversationID != old.Owner.ProviderConversationID || actor.BranchID != old.Owner.BranchID) {
		return ports.ErrAgentSettingsConflict
	}
	if current.IsTerminated || old.Version != expected || old.Owner != next.Owner || old.Revision != next.Revision || old.ConversationID != next.ConversationID || !slices.Equal(old.TurnIDs, next.TurnIDs) || !slices.Equal(old.InputRefs, next.InputRefs) || !old.CreatedAt.Equal(next.CreatedAt) || next.UpdatedAt.Before(old.UpdatedAt) || next.LastProgressAt.Before(old.LastProgressAt) || old.PreparedTurnID != "" && next.PreparedTurnID != old.PreparedTurnID || old.ExecutionSettled && !next.ExecutionSettled {
		return ports.ErrAgentSettingsConflict
	}
	if previousBoot && (current.Harness != domain.HarnessCodex || old.ExecutionSettled || old.Receipt == nil || next.Receipt == nil || *old.Receipt != *next.Receipt) {
		return ports.ErrAgentSettingsConflict
	}
	if !previousBoot && !old.ExecutionSettled && next.ExecutionSettled && (next.Receipt == nil || !next.Receipt.ExecutionSettled) {
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
	if next.Receipt != nil && !previousBoot {
		proof, err := json.Marshal(next.Receipt)
		if err != nil {
			return err
		}
		return q.InsertChatDeliveryReceipt(ctx, gen.InsertChatDeliveryReceiptParams{AttemptID: next.ID, Version: next.Version, ReceiptJson: string(proof), RecordedAt: next.UpdatedAt})
	}
	return nil
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
		return nil, &ports.ChatDeliveryJournalError{Stage: "read", Cause: err}
	}
	result := make([]domain.ChatDeliveryAttempt, 0, len(rows))
	for _, row := range rows {
		a, err := decodeChatDelivery(row)
		if err != nil {
			return nil, &ports.ChatDeliveryJournalError{Stage: "validate", Cause: err}
		}
		result = append(result, a)
	}
	return result, nil
}
