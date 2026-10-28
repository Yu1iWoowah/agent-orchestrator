package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func appendPlanTurn(t *testing.T, s *sqlite.Store, conversation string, session domain.SessionID, id string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.AppendUserMessage(ctx, conversation, session, "gen-1", domain.ConversationMessage{
		ID: "message-" + id, Text: id, ClientMessageID: "client-" + id, Origin: domain.MessageOriginHuman,
	}, id, now); err != nil {
		t.Fatal(err)
	}
	if err := s.BindTurnToProvider(ctx, id, "provider-"+id, now); err != nil {
		t.Fatal(err)
	}
}

func writePlan(t *testing.T, s *sqlite.Store, conversation, turn string, plan domain.ConversationPlan, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.SetTurnPlan(ctx, conversation, "provider-"+turn, plan); err != nil {
		t.Fatal(err)
	}
	detail, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertActivity(ctx, conversation, "provider-"+turn, domain.ConversationActivity{
		ID: "activity-" + turn, ProviderItemID: "ao-plan-provider-" + turn,
		Kind: domain.ActivityKindPlan, Status: domain.ActivityStatusRunning, Detail: detail,
	}, now); err != nil {
		t.Fatal(err)
	}
}

func TestConversationPlansReadBeyondTheTranscriptPage(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	appendPlanTurn(t, s, conversation, session, "first", histClock)
	writePlan(t, s, conversation, "first", domain.ConversationPlan{SpansTurns: true, Steps: []domain.ConversationPlanStep{
		{Text: "keep this session plan", Status: domain.PlanStepInProgress},
	}}, histClock.Add(time.Second))
	for i := range 250 {
		id := fmt.Sprintf("later-%d", i)
		appendPlanTurn(t, s, conversation, session, id, histClock.Add(time.Duration(i+2)*time.Second))
	}
	page, err := s.LoadConversationSnapshotPage(ctx, conversation, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Activities {
		if item.Kind == domain.ActivityKindPlan {
			t.Fatal("fixture did not put the plan outside the loaded transcript page")
		}
	}
	plans, err := s.ConversationPlans(ctx, conversation, 0, 1)
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans = %+v, %v", plans, err)
	}
	if plans[0].Plan.Steps[0].Text != "keep this session plan" || !plans[0].Plan.SpansTurns || plans[0].TurnID != "first" {
		t.Fatalf("latest plan = %+v", plans[0])
	}
}

func TestConversationPlansKeepAnExplicitClearAndPageHistory(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	appendPlanTurn(t, s, conversation, session, "first", histClock)
	writePlan(t, s, conversation, "first", domain.ConversationPlan{Steps: []domain.ConversationPlanStep{
		{Text: "older", Status: domain.PlanStepCompleted},
	}}, histClock.Add(time.Second))
	appendPlanTurn(t, s, conversation, session, "second", histClock.Add(2*time.Second))
	writePlan(t, s, conversation, "second", domain.ConversationPlan{Steps: []domain.ConversationPlanStep{}}, histClock.Add(3*time.Second))
	latest, err := s.ConversationPlans(ctx, conversation, 0, 1)
	if err != nil || len(latest) != 1 || len(latest[0].Plan.Steps) != 0 || latest[0].TurnID != "second" {
		t.Fatalf("cleared latest = %+v, %v", latest, err)
	}
	older, err := s.ConversationPlans(ctx, conversation, latest[0].Sequence, 1)
	if err != nil || len(older) != 1 || older[0].Plan.Steps[0].Text != "older" {
		t.Fatalf("history before clear = %+v, %v", older, err)
	}
}

func TestConversationPlansRepeatedPayloadKeepsRevision(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	appendPlanTurn(t, s, conversation, session, "first", histClock)
	plan := domain.ConversationPlan{Steps: []domain.ConversationPlanStep{{Text: "one", Status: domain.PlanStepPending}}}
	writePlan(t, s, conversation, "first", plan, histClock.Add(time.Second))
	first, err := s.ConversationPlans(ctx, conversation, 0, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("initial plan = %+v, %v", first, err)
	}
	writePlan(t, s, conversation, "first", plan, histClock.Add(2*time.Second))
	replayed, err := s.ConversationPlans(ctx, conversation, 0, 10)
	if err != nil || len(replayed) != 1 || replayed[0].Revision != first[0].Revision {
		t.Fatalf("same plan changed revision: before=%+v after=%+v err=%v", first, replayed, err)
	}
}

func TestConversationPlansReadLegacyTurnAndUnattributedActivity(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	appendPlanTurn(t, s, conversation, session, "legacy", histClock)
	if _, err := s.SetTurnPlan(ctx, conversation, "provider-legacy", domain.ConversationPlan{Steps: []domain.ConversationPlanStep{
		{Text: "legacy turn only", Status: domain.PlanStepPending},
	}}); err != nil {
		t.Fatal(err)
	}
	legacy, err := s.ConversationPlans(ctx, conversation, 0, 10)
	if err != nil || len(legacy) != 1 || legacy[0].TurnID != "legacy" || legacy[0].SourceActivityID != "" {
		t.Fatalf("legacy plan = %+v, %v", legacy, err)
	}
	writePlan(t, s, conversation, "unrecorded", domain.ConversationPlan{Steps: []domain.ConversationPlanStep{
		{Text: "recovered without a turn", Status: domain.PlanStepInProgress},
	}}, histClock.Add(time.Second))
	latest, err := s.ConversationPlans(ctx, conversation, 0, 1)
	if err != nil || len(latest) != 1 || latest[0].TurnID != "" || latest[0].SourceActivityID != "activity-unrecorded" {
		t.Fatalf("unattributed plan = %+v, %v", latest, err)
	}
}

func TestConversationPlansFollowBranchCutoffSwitchAndRollback(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	root, err := s.ConversationForSession(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	rootBranch, err := s.ConversationBranch(ctx, conversation, root.ActiveBranchID)
	if err != nil {
		t.Fatal(err)
	}
	appendPlanTurn(t, s, conversation, session, "first", histClock)
	writePlan(t, s, conversation, "first", domain.ConversationPlan{Steps: []domain.ConversationPlanStep{{Text: "ancestor", Status: domain.PlanStepCompleted}}}, histClock)
	first, err := s.ConversationPlans(ctx, conversation, 0, 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("ancestor = %+v, %v", first, err)
	}
	appendPlanTurn(t, s, conversation, session, "second", histClock.Add(time.Second))
	writePlan(t, s, conversation, "second", domain.ConversationPlan{Steps: []domain.ConversationPlanStep{{Text: "root future", Status: domain.PlanStepPending}}}, histClock.Add(time.Second))
	child := domain.ConversationBranch{ID: "plan-child", ConversationID: conversation, SessionID: session,
		ProviderConversationID: "thread-child", ParentBranchID: root.ActiveBranchID, ForkAfterSequence: first[0].Sequence}
	if err := s.CreateConversationBranch(ctx, child, histClock.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateConversationBranch(ctx, session, conversation, child.ID, child.ProviderConversationID, "gen-child", histClock); err != nil {
		t.Fatal(err)
	}
	inherited, err := s.ConversationPlans(ctx, conversation, 0, 10)
	if err != nil || len(inherited) != 1 || inherited[0].Plan.Steps[0].Text != "ancestor" {
		t.Fatalf("fork leaked a later parent plan: %+v, %v", inherited, err)
	}
	if _, err := s.AppendUserMessage(ctx, conversation, session, "gen-child", domain.ConversationMessage{
		ID: "message-child", Text: "child", Origin: domain.MessageOriginHuman, ClientMessageID: "client-child",
	}, "child", histClock.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.BindTurnToProvider(ctx, "child", "provider-child", histClock); err != nil {
		t.Fatal(err)
	}
	writePlan(t, s, conversation, "child", domain.ConversationPlan{Steps: []domain.ConversationPlanStep{{Text: "child plan", Status: domain.PlanStepPending}}}, histClock.Add(3*time.Second))
	if err := s.ActivateConversationBranch(ctx, session, conversation, root.ActiveBranchID, rootBranch.ProviderConversationID, "gen-root-again", histClock); err != nil {
		t.Fatal(err)
	}
	back, err := s.ConversationPlans(ctx, conversation, 0, 10)
	if err != nil || len(back) != 2 || back[0].Plan.Steps[0].Text != "root future" {
		t.Fatalf("switch leaked child: %+v, %v", back, err)
	}
	if _, err := s.RollbackTurns(ctx, conversation, "second", histClock.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := s.ConversationPlans(ctx, conversation, 0, 10)
	if err != nil || len(rolledBack) != 1 || rolledBack[0].Plan.Steps[0].Text != "ancestor" {
		t.Fatalf("rollback returned discarded plan: %+v, %v", rolledBack, err)
	}
}

func TestConversationPlansUseAtomicGenerationFencedProjection(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	appendPlanTurn(t, s, conversation, session, "first", histClock)
	plan := domain.ConversationPlan{Steps: []domain.ConversationPlanStep{{Text: "accepted", Status: domain.PlanStepPending}}}
	project := func(ctx context.Context) error {
		if _, err := s.SetTurnPlan(ctx, conversation, "provider-first", plan); err != nil {
			return err
		}
		detail, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		return s.UpsertActivity(ctx, conversation, "provider-first", domain.ConversationActivity{
			ID: "atomic-plan", ProviderItemID: "ao-plan-provider-first", Kind: domain.ActivityKindPlan,
			Status: domain.ActivityStatusRunning, Detail: detail,
		}, histClock)
	}
	_, err := s.ProjectProviderEvent(ctx, conversation, session, "gen-1", "failed-event", "plan.updated", `{}`, histClock, func(ctx context.Context) error {
		if err := project(ctx); err != nil {
			return err
		}
		return errors.New("injected projection failure")
	})
	if err == nil {
		t.Fatal("injected failure was accepted")
	}
	failed, err := s.ConversationPlans(ctx, conversation, 0, 10)
	if err != nil || len(failed) != 0 {
		t.Fatalf("partial plan survived rollback: %+v, %v", failed, err)
	}
	if applied, err := s.ProjectProviderEvent(ctx, conversation, session, "stale-generation", "old-event", "plan.updated", `{}`, histClock, project); err != nil || applied {
		t.Fatalf("stale generation applied = %v, %v", applied, err)
	}
	if applied, err := s.ProjectProviderEvent(ctx, conversation, session, "gen-1", "plan-event", "plan.updated", `{}`, histClock, project); err != nil || !applied {
		t.Fatalf("current generation applied = %v, %v", applied, err)
	}
	if applied, err := s.ProjectProviderEvent(ctx, conversation, session, "gen-1", "plan-event", "plan.updated", `{}`, histClock, func(context.Context) error {
		t.Fatal("replay reprojected the plan")
		return nil
	}); err != nil || applied {
		t.Fatalf("replay applied = %v, %v", applied, err)
	}
	accepted, err := s.ConversationPlans(ctx, conversation, 0, 10)
	if err != nil || len(accepted) != 1 || accepted[0].Plan.Steps[0].Text != "accepted" {
		t.Fatalf("accepted plan = %+v, %v", accepted, err)
	}
}

func TestConversationPlansOrderByAcceptedUpdateRatherThanFirstActivity(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	appendPlanTurn(t, s, conversation, session, "first", histClock)
	writePlan(t, s, conversation, "first", domain.ConversationPlan{Steps: []domain.ConversationPlanStep{{Text: "first plan", Status: domain.PlanStepPending}}}, histClock.Add(time.Second))
	appendPlanTurn(t, s, conversation, session, "second", histClock.Add(2*time.Second))
	writePlan(t, s, conversation, "second", domain.ConversationPlan{Steps: []domain.ConversationPlanStep{{Text: "second plan", Status: domain.PlanStepPending}}}, histClock.Add(3*time.Second))
	writePlan(t, s, conversation, "first", domain.ConversationPlan{Steps: []domain.ConversationPlanStep{{Text: "latest accepted replan", Status: domain.PlanStepPending}}}, histClock.Add(4*time.Second))
	plans, err := s.ConversationPlans(ctx, conversation, 0, 1)
	if err != nil || len(plans) != 1 || plans[0].Plan.Steps[0].Text != "latest accepted replan" {
		t.Fatalf("latest = %+v, %v", plans, err)
	}
}

func TestConversationPlansCompletionDoesNotPromoteAnOlderSource(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	appendPlanTurn(t, s, conversation, session, "first", histClock)
	writePlan(t, s, conversation, "first", domain.ConversationPlan{Steps: []domain.ConversationPlanStep{{Text: "older", Status: domain.PlanStepPending}}}, histClock)
	appendPlanTurn(t, s, conversation, session, "second", histClock.Add(time.Second))
	writePlan(t, s, conversation, "second", domain.ConversationPlan{SpansTurns: true, Steps: []domain.ConversationPlanStep{{Text: "current", Status: domain.PlanStepPending}}}, histClock.Add(time.Second))
	if err := s.SettleTurn(ctx, conversation, "provider-first", domain.TurnStateCompleted, "", histClock.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	plans, err := s.ConversationPlans(ctx, conversation, 0, 10)
	if err != nil || len(plans) != 2 || plans[0].TurnID != "second" || plans[0].Plan.Steps[0].Status != domain.PlanStepPending || plans[1].Plan.Steps[0].Status != domain.PlanStepCompleted {
		t.Fatalf("completed older source = %+v, %v", plans, err)
	}
}

func TestConversationPlansPreserveARevisionBeforeTheForkCutoff(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	root, err := s.ConversationForSession(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	appendPlanTurn(t, s, conversation, session, "first", histClock)
	writePlan(t, s, conversation, "first", domain.ConversationPlan{SpansTurns: true, Steps: []domain.ConversationPlanStep{{Text: "before fork", Status: domain.PlanStepPending}}}, histClock)
	before, err := s.ConversationPlans(ctx, conversation, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	writePlan(t, s, conversation, "first", domain.ConversationPlan{SpansTurns: true, Steps: []domain.ConversationPlanStep{{Text: "parent future revision", Status: domain.PlanStepPending}}}, histClock.Add(time.Second))
	child := domain.ConversationBranch{ID: "revision-child", ConversationID: conversation, SessionID: session, ProviderConversationID: "revision-thread", ParentBranchID: root.ActiveBranchID, ForkAfterSequence: before[0].Sequence}
	if err := s.CreateConversationBranch(ctx, child, histClock); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateConversationBranch(ctx, session, conversation, child.ID, child.ProviderConversationID, "revision-generation", histClock); err != nil {
		t.Fatal(err)
	}
	plans, err := s.ConversationPlans(ctx, conversation, 0, 10)
	if err != nil || len(plans) != 1 || plans[0].Plan.Steps[0].Text != "before fork" {
		t.Fatalf("fork revision = %+v, %v", plans, err)
	}
}

func TestConversationPlansPreserveLegacyTurnBeforeItsFirstActivity(t *testing.T) {
	s, session, conversation := conversationFixture(t)
	ctx := context.Background()
	root, err := s.ConversationForSession(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	appendPlanTurn(t, s, conversation, session, "legacy-first", histClock)
	_, err = s.SetTurnPlan(ctx, conversation, "provider-legacy-first", domain.ConversationPlan{SpansTurns: true, Steps: []domain.ConversationPlanStep{{Text: "legacy before update", Status: domain.PlanStepPending}}})
	if err != nil {
		t.Fatal(err)
	}
	// Do not read the Plan API first: the first update must preserve old data too.
	writePlan(t, s, conversation, "legacy-first", domain.ConversationPlan{SpansTurns: true, Steps: []domain.ConversationPlanStep{{Text: "new activity plan", Status: domain.PlanStepPending}}}, histClock.Add(time.Second))
	child := domain.ConversationBranch{ID: "legacy-revision-child", ConversationID: conversation, SessionID: session, ProviderConversationID: "legacy-revision-thread", ParentBranchID: root.ActiveBranchID, ForkAfterSequence: 1}
	if err := s.CreateConversationBranch(ctx, child, histClock); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateConversationBranch(ctx, session, conversation, child.ID, child.ProviderConversationID, "legacy-revision-generation", histClock); err != nil {
		t.Fatal(err)
	}
	plans, err := s.ConversationPlans(ctx, conversation, 0, 10)
	if err != nil || len(plans) != 1 || plans[0].Plan.Steps[0].Text != "legacy before update" {
		t.Fatalf("legacy fork revision = %+v, %v", plans, err)
	}
}
