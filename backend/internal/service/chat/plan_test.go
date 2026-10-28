package chat_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestSessionPlanDistinguishesAbsentAndExplicitClear(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	absent, err := h.svc.Plan(ctx, testSession)
	if err != nil || absent.Version != nil {
		t.Fatalf("absent plan = %+v, %v", absent, err)
	}
	if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "make a plan", Origin: domain.MessageOriginHuman}); err != nil {
		t.Fatal(err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventPlanUpdated, ProviderTurnID: "provider-turn-1",
		Plan: &domain.ConversationPlan{SpansTurns: true, Steps: []domain.ConversationPlanStep{{Text: "work", Status: domain.PlanStepInProgress}}}})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return countActivities(s, domain.ActivityKindPlan) == 1 })
	present, err := h.svc.Plan(ctx, testSession)
	if err != nil || present.Version == nil || present.Version.Plan.Steps[0].Text != "work" || !present.Version.Plan.SpansTurns {
		t.Fatalf("present plan = %+v, %v", present, err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventPlanUpdated, ProviderTurnID: "provider-turn-1",
		Plan: &domain.ConversationPlan{Steps: []domain.ConversationPlanStep{}}})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		for _, turn := range s.Turns {
			if turn.Plan != nil && len(turn.Plan.Steps) == 0 {
				return true
			}
		}
		return false
	})
	cleared, err := h.svc.Plan(ctx, testSession)
	if err != nil || cleared.Version == nil || len(cleared.Version.Plan.Steps) != 0 {
		t.Fatalf("explicit clear disappeared = %+v, %v", cleared, err)
	}
}

func TestSessionPlanHistoryReturnsBoundedVersionsAndCursor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i, text := range []string{"first", "second", "third"} {
		if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: text, Origin: domain.MessageOriginHuman}); err != nil {
			t.Fatal(err)
		}
		providerTurn := "provider-turn-" + string(rune('1'+i))
		h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventPlanUpdated, ProviderTurnID: providerTurn,
			Plan: &domain.ConversationPlan{Steps: []domain.ConversationPlanStep{{Text: text, Status: domain.PlanStepPending}}}})
		h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return countActivities(s, domain.ActivityKindPlan) == i+1 })
		h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: providerTurn, TurnState: domain.TurnStateCompleted})
		h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
			return len(s.Turns) > i && s.Turns[i].State == domain.TurnStateCompleted
		})
	}
	page, err := h.svc.PlanHistory(ctx, testSession, 0, 2)
	if err != nil || len(page.Versions) != 2 || !page.HasMoreBefore || page.Versions[0].Plan.Steps[0].Text != "third" {
		t.Fatalf("first history page = %+v, %v", page, err)
	}
	older, err := h.svc.PlanHistory(ctx, testSession, page.NextBeforeSequence, 2)
	if err != nil || len(older.Versions) != 1 || older.HasMoreBefore || older.Versions[0].Plan.Steps[0].Text != "first" {
		t.Fatalf("older history page = %+v, %v", older, err)
	}
}

func TestUnattributedPlanKeepsItsCrossTurnScope(t *testing.T) {
	h := newHarness(t)
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventPlanUpdated, ProviderTurnID: "prior-controller-turn",
		Plan: &domain.ConversationPlan{SpansTurns: true, Steps: []domain.ConversationPlanStep{{Text: "recovered list", Status: domain.PlanStepPending}}}})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return countActivities(s, domain.ActivityKindPlan) == 1 })
	plan, err := h.svc.Plan(context.Background(), testSession)
	if err != nil || plan.Version == nil || plan.Version.TurnID != "" || !plan.Version.Plan.SpansTurns {
		t.Fatalf("unattributed plan lost scope = %+v, %v", plan, err)
	}
}
