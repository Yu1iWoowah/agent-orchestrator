package store_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestPreviousBootAcceptanceKeepsIncompleteBatchMembersOutOfQueue(t *testing.T) {
	s, db, rec := settingsStartFixture(t, "")
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, "UPDATE sessions SET harness=? WHERE id=?", domain.HarnessCodex, rec.ID); err != nil {
		t.Fatal(err)
	}
	settings, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := s.ConversationForSession(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	owner := *dispatchSnapshot(t, settings).Owner
	for _, id := range []string{"root", "follower"} {
		if _, err := s.AppendUserMessage(ctx, conv.ID, rec.ID, settings.OwnerGeneration, domain.ConversationMessage{ID: "message-" + id, Text: "fixture", ClientMessageID: "client-" + id, Origin: domain.MessageOriginHuman}, id, at); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := s.ClaimQueuedBatch(ctx, conv.ID, true, 2, at)
	if err != nil || len(batch) != 2 {
		t.Fatalf("queued batch = %v, %v", batch, err)
	}
	ids := []string{batch[0].TurnID, batch[1].TurnID}
	before, created, err := s.ReserveChatDelivery(ctx, domain.ChatDeliveryAttempt{ID: "old-batch", SessionID: rec.ID, ConversationID: conv.ID, Owner: owner, Revision: settings.Revision, TurnIDs: ids, Disposition: domain.ChatDeliveryReserved, CreatedAt: at, UpdatedAt: at, LastProgressAt: at})
	if err != nil || !created {
		t.Fatalf("reserve batch = %t, %v", created, err)
	}
	const providerTurn = "original-native-turn"
	if err := s.BindTurnToProvider(ctx, ids[0], providerTurn, at); err != nil {
		t.Fatal(err)
	}
	proof := domain.ChatDeliveryReceipt{AttemptID: before.ID, ProviderTurnID: providerTurn, ProviderRequestID: "original-request", ProviderEpoch: "previous-boot", Disposition: domain.ChatDeliveryAccepted, Source: "provider_response", Complete: true, ObservedAt: at}
	before.Version, before.EvidenceLevel, before.Disposition, before.Receipt = 1, 2, domain.ChatDeliveryAccepted, &proof
	before, err = s.UpdateChatDelivery(ctx, before, 0, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SettlePreviousBootAcceptance(ctx, before, owner, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	saved, err := s.ChatDeliveryAttempt(ctx, rec.ID, before.ID)
	if err != nil || !saved.ExecutionSettled || saved.Receipt == nil || *saved.Receipt != proof || !slices.Equal(saved.InputRefs, before.InputRefs) {
		t.Fatalf("accepted batch changed: %+v, %v", saved, err)
	}
	for i, want := range []domain.TurnState{domain.TurnStateInterrupted, domain.TurnStateCompleted} {
		turn, err := s.TurnByID(ctx, ids[i])
		if err != nil || turn.State != want {
			t.Fatalf("input %s state = %s, want %s: %v", ids[i], turn.State, want, err)
		}
	}
	if _, err := s.NextQueuedTurn(ctx, conv.ID); !errors.Is(err, domain.ErrNoQueuedTurn) {
		t.Fatalf("settled batch became eligible for redispatch: %v", err)
	}
}

func TestChatDeliveryReservationKeepsOneExecutionAndMonotonicEvidence(t *testing.T) {
	s, db, rec := settingsStartFixture(t, "")
	current, err := s.SessionAgentSettings(t.Context(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := s.ConversationForSession(t.Context(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	owner := *dispatchSnapshot(t, current).Owner
	if _, err = s.AppendUserMessage(t.Context(), conv.ID, rec.ID, current.OwnerGeneration, domain.ConversationMessage{ID: "delivery-message", Text: "synthetic input", ClientMessageID: "delivery-client", Origin: domain.MessageOriginHuman}, "delivery-turn", at); err != nil {
		t.Fatal(err)
	}
	base := domain.ChatDeliveryAttempt{SessionID: rec.ID, ConversationID: conv.ID, Owner: owner, Revision: current.Revision, TurnIDs: []string{"delivery-turn"}, Disposition: domain.ChatDeliveryReserved, CreatedAt: at, UpdatedAt: at, LastProgressAt: at}
	var wg sync.WaitGroup
	results := make(chan domain.ChatDeliveryAttempt, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := base
			a.ID = fmt.Sprintf("delivery-%d", i)
			saved, created, err := s.ReserveChatDelivery(t.Context(), a)
			if err == nil && created {
				results <- saved
			} else if !errors.Is(err, ports.ErrAgentSettingsConflict) {
				t.Errorf("reserve: %v", err)
			}
		}(i)
	}
	wg.Wait()
	close(results)
	var a domain.ChatDeliveryAttempt
	count := 0
	for saved := range results {
		a = saved
		count++
	}
	if count != 1 {
		t.Fatalf("reserved executions=%d", count)
	}
	r := domain.ChatDeliveryReceipt{AttemptID: a.ID, ProviderTurnID: "native-turn", ProviderRequestID: "request-1", ProviderEpoch: "epoch-1", Disposition: domain.ChatDeliveryAccepted, Source: "provider_response", Complete: true, ObservedAt: at}
	a.Version++
	a.Disposition = domain.ChatDeliveryAccepted
	a.EvidenceLevel = 2
	a.Receipt = &r
	a, err = s.UpdateChatDelivery(t.Context(), a, 0, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SettleTurnByID(t.Context(), "delivery-turn", domain.TurnStateFailed, "synthetic execution failed", at); err != nil {
		t.Fatal(err)
	}
	// Even an older cleanup that incorrectly makes the input queued cannot
	// erase its original execution's delivery fact.
	if _, err = db.Exec("UPDATE conversation_turns SET state='queued',completed_at=NULL,error_message='' WHERE id='delivery-turn'"); err != nil {
		t.Fatal(err)
	}
	base.ID = "second-execution"
	if _, _, err = s.ReserveChatDelivery(t.Context(), base); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("cleanup permitted replay: %v", err)
	}
	base.ID = a.ID
	if original, created, err := s.ReserveChatDelivery(t.Context(), base); err != nil || created || original.Disposition != domain.ChatDeliveryAccepted {
		t.Fatalf("same-key recovery gained dispatch: %+v created=%v err=%v", original, created, err)
	}
	bad := a
	bad.Version++
	bad.Disposition = domain.ChatDeliveryNotSent
	bad.EvidenceLevel = 0
	proof := r
	proof.Disposition = domain.ChatDeliveryNotSent
	proof.Source = "not_forwarded"
	bad.Receipt = &proof
	if _, err = s.UpdateChatDelivery(t.Context(), bad, a.Version, owner); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("accepted downgrade: %v", err)
	}
	got, err := s.ChatDeliveryAttempt(t.Context(), rec.ID, a.ID)
	if err != nil || got.Disposition != domain.ChatDeliveryAccepted || got.EvidenceLevel != 2 {
		t.Fatalf("lost acceptance: %+v %v", got, err)
	}
}

func TestSettingsReconciliationClaimNeverRepeatsUnknownEffect(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			s, db, rec := settingsStartFixture(t, "")
			current, err := s.SessionAgentSettings(t.Context(), rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC()
			r := domain.AgentSettingsReconciliation{ID: "reconciliation", SessionID: rec.ID, Owner: *dispatchSnapshot(t, current).Owner, Revision: current.Revision, Phase: "observing", Step: "observing", ExecutorID: "executor-1", ClaimUntil: at.Add(time.Second), Deadline: at.Add(30 * time.Second), CreatedAt: at, LastProgressAt: at, Desired: current.Settings, PreferenceAtStart: current.Settings, ObservationAvailability: "unknown", MissingEvidence: []string{"current_settings"}, RecoveryActions: []string{"retry"}}
			r, err = s.BeginAgentSettingsReconciliation(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			duplicate := r
			duplicate.ID = "duplicate"
			got, err := s.BeginAgentSettingsReconciliation(t.Context(), duplicate)
			if err != nil || got.ID != r.ID {
				t.Fatalf("dedup: %+v %v", got, err)
			}
			if pending {
				r.Version++
				r.Step = "applying_model"
				r.Phase = "applying"
				r.ActionKey = "reconciliation:model"
				r.EffectPending = true
				r, err = s.SaveAgentSettingsReconciliation(t.Context(), r, 0, "executor-1")
				if err != nil {
					t.Fatal(err)
				}
			}
			claimed, err := s.ClaimAgentSettingsReconciliation(t.Context(), rec.ID, r.ID, r.Version, "executor-2", at.Add(2*time.Second))
			if pending {
				if !errors.Is(err, ports.ErrAgentSettingsConflict) {
					t.Fatalf("claimed uncertain setter: %+v %v", claimed, err)
				}
				return
			}
			if err != nil || claimed.ID != r.ID || claimed.ExecutorID != "executor-2" {
				t.Fatalf("readonly takeover: %+v %v", claimed, err)
			}
			stale := r
			stale.Version++
			if _, err = s.SaveAgentSettingsReconciliation(t.Context(), stale, r.Version, "executor-1"); !errors.Is(err, ports.ErrAgentSettingsConflict) {
				t.Fatalf("old executor committed: %v", err)
			}
			bad := claimed
			bad.Version++
			bad.SafeCause = "credential-sentinel-private"
			if _, err = s.SaveAgentSettingsReconciliation(t.Context(), bad, claimed.Version, "executor-2"); !errors.Is(err, ports.ErrAgentSettingsInvalid) {
				t.Fatalf("unsafe diagnostic: %v", err)
			}
			var raw string
			if err = db.QueryRow("SELECT record_json FROM agent_settings_reconciliations WHERE id=?", r.ID).Scan(&raw); err != nil || strings.Contains(raw, "credential-sentinel") {
				t.Fatalf("unsafe persistence: %s %v", raw, err)
			}
		})
	}
}

func TestTerminalDeliveryWithdrawsAttentionAtomically(t *testing.T) {
	for _, disposition := range []domain.ChatDeliveryDisposition{domain.ChatDeliveryAccepted, domain.ChatDeliveryNotSent} {
		t.Run(string(disposition), func(t *testing.T) {
			s, _, rec := settingsStartFixture(t, "")
			current, err := s.SessionAgentSettings(t.Context(), rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			conv, err := s.ConversationForSession(t.Context(), rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC()
			owner := *dispatchSnapshot(t, current).Owner
			if _, err := s.AppendUserMessage(t.Context(), conv.ID, rec.ID, current.OwnerGeneration, domain.ConversationMessage{ID: "message", Text: "preserved input", ClientMessageID: "input", Origin: domain.MessageOriginHuman}, "turn", at); err != nil {
				t.Fatal(err)
			}
			a, _, err := s.ReserveChatDelivery(t.Context(), domain.ChatDeliveryAttempt{ID: "attempt", SessionID: rec.ID, ConversationID: conv.ID, Owner: owner, Revision: current.Revision, TurnIDs: []string{"turn"}, Disposition: domain.ChatDeliveryReserved, CreatedAt: at, UpdatedAt: at, LastProgressAt: at})
			if err != nil {
				t.Fatal(err)
			}
			command := askCreate(rec.ID)
			command.ClientKey = "delivery-" + a.ID
			question, _, err := s.CreateAttentionQuestion(t.Context(), command, at)
			if err != nil {
				t.Fatal(err)
			}
			a.Version++
			a.AttentionID = question.ID
			a.BlockedSince = &at
			a.ErrorCode = "CHAT_EXECUTION_STATE_UNVERIFIED"
			a, err = s.UpdateChatDelivery(t.Context(), a, 0, owner)
			if err != nil {
				t.Fatal(err)
			}
			receipt := domain.ChatDeliveryReceipt{AttemptID: a.ID, ProviderEpoch: "epoch", ProviderRequestID: "rpc", Disposition: disposition, Complete: true, Source: "not_forwarded", ObservedAt: at}
			if disposition == domain.ChatDeliveryAccepted {
				receipt.Source = "provider_response"
				receipt.ExecutionSettled = true
				a.ExecutionSettled = true
				a.EvidenceLevel = 2
			}
			a.Version++
			a.Disposition = disposition
			a.Receipt = &receipt
			a.BlockedSince = nil
			a.ErrorCode = ""
			if _, err = s.UpdateChatDelivery(t.Context(), a, 1, owner); err != nil {
				t.Fatal(err)
			}
			closed, err := s.GetAttentionQuestion(t.Context(), rec.ID, question.ID)
			if err != nil || closed.State != "cancelled" {
				t.Fatalf("terminal receipt left actionable attention: %+v %v", closed, err)
			}
		})
	}
}

func TestDeliveryPromptAndAttemptLinkCommitTogether(t *testing.T) {
	s, _, rec := settingsStartFixture(t, "")
	ctx := t.Context()
	current, err := s.SessionAgentSettings(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := s.ConversationForSession(ctx, rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	owner := *dispatchSnapshot(t, current).Owner
	if _, err := s.AppendUserMessage(ctx, conv.ID, rec.ID, current.OwnerGeneration, domain.ConversationMessage{ID: "message", Text: "preserved input", ClientMessageID: "input", Origin: domain.MessageOriginHuman}, "turn", at); err != nil {
		t.Fatal(err)
	}
	a, _, err := s.ReserveChatDelivery(ctx, domain.ChatDeliveryAttempt{ID: "attempt", SessionID: rec.ID, ConversationID: conv.ID, Owner: owner, Revision: current.Revision, TurnIDs: []string{"turn"}, Disposition: domain.ChatDeliveryReserved, CreatedAt: at, UpdatedAt: at, LastProgressAt: at})
	if err != nil {
		t.Fatal(err)
	}
	a.Version++
	a.BlockedSince = &at
	a.ErrorCode = "CHAT_EXECUTION_STATE_UNVERIFIED"
	prompt := askCreate(rec.ID)
	prompt.ClientKey = "delivery-" + a.ID + "-1"
	// An owner CAS failure on the link rolls the prompt back with it.
	stale := owner
	stale.BranchID = "other-branch"
	if _, err := s.BlockChatDeliveryWithAttention(ctx, a, 0, stale, prompt, at); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("stale owner block = %v", err)
	}
	if _, err := s.GetAttentionQuestion(ctx, rec.ID, prompt.ID); !errors.Is(err, domain.ErrAttentionQuestionNotFound) {
		t.Fatalf("delivery prompt became visible without its attempt link: %v", err)
	}
	if unchanged, err := s.ChatDeliveryAttempt(ctx, rec.ID, a.ID); err != nil || unchanged.Version != 0 || unchanged.AttentionID != "" || unchanged.BlockedSince != nil {
		t.Fatalf("failed block changed the attempt: %+v %v", unchanged, err)
	}
	linked, err := s.BlockChatDeliveryWithAttention(ctx, a, 0, owner, prompt, at)
	if err != nil || linked.AttentionID != prompt.ID {
		t.Fatalf("retried block = %+v %v", linked, err)
	}
	stored, err := s.ChatDeliveryAttempt(ctx, rec.ID, a.ID)
	if err != nil || stored.AttentionID != prompt.ID || stored.Version != 1 {
		t.Fatalf("stored link = %+v %v", stored, err)
	}
	receipt, err := s.AnswerAttentionQuestion(ctx, rec.ID, askAnswer(prompt.ID), at)
	if err != nil || receipt.DeliveryState != "recorded" {
		t.Fatalf("answer to the linked prompt = %+v %v", receipt, err)
	}
}
