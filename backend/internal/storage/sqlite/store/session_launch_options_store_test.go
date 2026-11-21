package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestSessionLaunchOptionsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "launch")
	session, err := s.CreateSession(ctx, sampleRecord("launch"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if got, err := s.SessionLaunchOptions(ctx, session.ID); err != nil || got != (domain.AgentLaunchOptions{}) {
		t.Fatalf("new session launch options = %+v, %v; want none", got, err)
	}
	want := domain.AgentLaunchOptions{FastMode: true, Ultracode: true, AutoCompactWindow: "200k"}
	if err := s.SetSessionLaunchOptions(ctx, session.ID, want); err != nil {
		t.Fatalf("set launch options: %v", err)
	}
	if got, err := s.SessionLaunchOptions(ctx, session.ID); err != nil || got != want {
		t.Fatalf("launch options = %+v, %v; want %+v", got, err, want)
	}
	if err := s.SetSessionLaunchOptions(ctx, session.ID, domain.AgentLaunchOptions{}); err != nil {
		t.Fatalf("clear launch options: %v", err)
	}
	if got, _ := s.SessionLaunchOptions(ctx, session.ID); got != (domain.AgentLaunchOptions{}) {
		t.Fatalf("cleared launch options = %+v; want none", got)
	}
}

func TestTUIProviderSwitchPublishesAndRollsBackOnlyItsFirstAccountBinding(t *testing.T) {
	s := newTestStore(t)
	ctx := t.Context()
	seedProject(t, s, "launch")
	source, err := s.CreateSession(ctx, sampleRecord("launch"))
	if err != nil {
		t.Fatal(err)
	}
	sourceAccount := domain.ResolvedAgentAccount{
		AccountID: "device-claude", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceDevice,
	}
	at := time.Date(2026, 11, 21, 1, 2, 3, 42, time.UTC)
	if err := s.BindSessionAgentAccountWithSource(ctx, source.ID, sourceAccount, at); err != nil {
		t.Fatal(err)
	}
	target := source
	target.Harness = domain.HarnessCodex
	targetAccount := domain.ResolvedAgentAccount{
		AccountID: "acct-target", Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceRole,
	}
	options := domain.AgentLaunchOptions{HandoffNote: "handoff"}
	if err := s.UpdateTUIProviderSwitch(ctx, target, options, targetAccount, at, false); err != nil {
		t.Fatal(err)
	}
	if binding, found, err := s.GetSessionAgentAccountBinding(ctx, source.ID, targetAccount.Provider); err != nil || !found || binding != targetAccount {
		t.Fatalf("target binding = %+v, found=%v, err=%v", binding, found, err)
	}
	if err := s.UpdateTUIProviderSwitch(ctx, source, domain.AgentLaunchOptions{}, targetAccount, at, true); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.GetSessionAgentAccountBinding(ctx, source.ID, targetAccount.Provider); err != nil || found {
		t.Fatalf("rolled back target binding: found=%v, err=%v", found, err)
	}
	if binding, found, err := s.GetSessionAgentAccountBinding(ctx, source.ID, sourceAccount.Provider); err != nil || !found || binding != sourceAccount {
		t.Fatalf("source binding = %+v, found=%v, err=%v", binding, found, err)
	}

	existing := targetAccount
	existing.AccountID = "acct-existing"
	if err := s.BindSessionAgentAccountWithSource(ctx, source.ID, existing, at); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTUIProviderSwitch(ctx, target, options, targetAccount, at, false); !errors.Is(err, ports.ErrAgentAccountChanged) {
		t.Fatalf("switch with a competing binding: %v", err)
	}
	if binding, found, err := s.GetSessionAgentAccountBinding(ctx, source.ID, existing.Provider); err != nil || !found || binding != existing {
		t.Fatalf("existing binding = %+v, found=%v, err=%v", binding, found, err)
	}
	current, _, err := s.GetSession(ctx, source.ID)
	if err != nil || current.Harness != source.Harness {
		t.Fatalf("rolled back harness = %q, err=%v", current.Harness, err)
	}
	if got, err := s.SessionLaunchOptions(ctx, source.ID); err != nil || got != (domain.AgentLaunchOptions{}) {
		t.Fatalf("rolled back launch options = %+v, err=%v", got, err)
	}
}

func TestUpdateSessionWithLaunchOptionsWritesBothOrNeither(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "launch")
	session, err := s.CreateSession(ctx, sampleRecord("launch"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	session.Harness = domain.HarnessCodex
	want := domain.AgentLaunchOptions{FastMode: true, HandoffNote: "note"}
	if err := s.UpdateSessionWithLaunchOptions(ctx, session, want); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _, _ := s.GetSession(ctx, session.ID)
	if options, _ := s.SessionLaunchOptions(ctx, session.ID); got.Harness != domain.HarnessCodex || options != want {
		t.Fatalf("harness %q, options %+v; want codex with %+v", got.Harness, options, want)
	}
	// A cancelled write moves neither.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	session.Harness = domain.HarnessClaudeCode
	if err := s.UpdateSessionWithLaunchOptions(cancelled, session, domain.AgentLaunchOptions{}); err == nil {
		t.Fatal("a cancelled write succeeded")
	}
	got, _, _ = s.GetSession(ctx, session.ID)
	if options, _ := s.SessionLaunchOptions(ctx, session.ID); got.Harness != domain.HarnessCodex || options != want {
		t.Fatalf("after a failed write: harness %q, options %+v", got.Harness, options)
	}
}
