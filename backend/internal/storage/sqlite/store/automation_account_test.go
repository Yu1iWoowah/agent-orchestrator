package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestAutomationAccountReferenceRoundTripsCreateUpdateAndDueRun(t *testing.T) {
	s := newTestStore(t)
	seedProject(t, s, "scheduled")
	ctx := context.Background()
	now := time.Now().UTC()
	a := domain.Automation{ID: "account-schedule", ProjectID: "scheduled", DisplayName: "fake", Prompt: "fake", Kind: domain.KindWorker, Harness: domain.HarnessCodex, AgentAccountID: "worker-A", RRuleText: "FREQ=DAILY", Timezone: "UTC", Enabled: true, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	if _, err := s.CreateAutomation(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.GetAutomation(ctx, a.ID)
	if err != nil || !found || got.AgentAccountID != "worker-A" {
		t.Fatalf("automation lost account: %+v %v", got, err)
	}
	a.AgentAccountID = "worker-B"
	if _, err := s.UpdateAutomation(ctx, a); err != nil {
		t.Fatal(err)
	}
	due, err := s.ListDueAutomations(ctx, now.Add(time.Second), 10)
	if err != nil || len(due) != 1 || due[0].AgentAccountID != "worker-B" {
		t.Fatalf("due run lost account override: %+v %v", due, err)
	}
}
