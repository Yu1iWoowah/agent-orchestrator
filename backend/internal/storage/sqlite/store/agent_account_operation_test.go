package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestAgentAccountJournalAtomicAcceptanceAndTombstone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	session := seedUsageSession(t, s, domain.HarnessClaudeCode)
	a := domain.AgentAccount{ID: "fake", Provider: domain.AgentAccountProviderClaude, Home: "/fake/profile", Email: "fake@example.invalid", CreatedAt: now, ProfileKind: domain.AgentAccountProfileContained}
	o := domain.AgentAccountOperation{ID: "login", AccountID: a.ID, Provider: a.Provider, Action: "login", Phase: "pending", CreatedAt: now, UpdatedAt: now}
	mustNoError(t, s.CreateAgentAccountOperation(ctx, o))
	o.Phase = "accepted"
	o.Outcome = "complete"
	o.ResultAccountID = a.ID
	mustNoError(t, s.CommitAgentAccountLogin(ctx, o, a))
	rows, e := s.ListAgentAccounts(ctx)
	mustNoError(t, e)
	if len(rows) != 1 || rows[0].DisabledAt == nil {
		t.Fatal("accepted login was enabled before terminal cleanup")
	}
	o.Phase = "complete"
	mustNoError(t, s.UpdateAgentAccountOperation(ctx, o))
	rows, e = s.ListAgentAccounts(ctx)
	mustNoError(t, e)
	if rows[0].DisabledAt != nil {
		t.Fatal("confirmed login stayed disabled")
	}
	mustNoError(t, s.BindSessionAgentAccount(ctx, session.ID, a.Provider, a.ID, now))
	mustNoError(t, s.SetAgentAccountDefault(ctx, a.Provider, a.ID))
	remove := domain.AgentAccountOperation{ID: "remove", AccountID: a.ID, Provider: a.Provider, Action: "remove", Phase: "closing", Outcome: "removed", CreatedAt: now, UpdatedAt: now}
	mustNoError(t, s.CreateAgentAccountOperation(ctx, remove))
	other := remove
	other.ID = "parallel"
	if e := s.CreateAgentAccountOperation(ctx, other); e == nil {
		t.Fatal("parallel destructive account operation accepted")
	}
	remove.Phase = "deleting"
	mustNoError(t, s.UpdateAgentAccountOperation(ctx, remove))
	rows, e = s.ListAgentAccounts(ctx)
	mustNoError(t, e)
	if len(rows) != 1 || rows[0].RetiredAt == nil || rows[0].Home != a.Home || rows[0].Email != a.Email {
		t.Fatal("removal erased durable historical metadata")
	}
	id, found, e := s.GetSessionAgentAccount(ctx, session.ID, a.Provider)
	mustNoError(t, e)
	if !found || id != a.ID {
		t.Fatal("retirement rewrote historical binding")
	}
	_, found, e = s.GetAgentAccountDefault(ctx, a.Provider)
	mustNoError(t, e)
	if found {
		t.Fatal("retired account remains default")
	}
}
