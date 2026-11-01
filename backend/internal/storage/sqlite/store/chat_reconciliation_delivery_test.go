package store_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

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
