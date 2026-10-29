package store_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestManualControlsAdvanceConversationUpdatedAt(t *testing.T) {
	s, _, rec := settingsFixture(t)
	before := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	conv, err := s.CreateConversation(t.Context(), "controls-time", domain.ConversationScopeSession, rec.ProjectID, rec.ID, before)
	if err != nil {
		t.Fatal(err)
	}
	at := before.Add(time.Hour)
	if err := s.SetConversationManualControls(t.Context(), conv.ID, domain.ConversationSettings{ApprovalMode: domain.PermissionModeAuto}, at); err != nil {
		t.Fatal(err)
	}
	saved, err := s.ConversationForSession(t.Context(), rec.ID)
	if err != nil || !saved.UpdatedAt.Equal(at) {
		t.Fatalf("manual controls left stale updated_at: %v %v", saved.UpdatedAt, err)
	}
}

func TestManualControlsDoNotOverwriteCommittedSettings(t *testing.T) {
	s, _, rec := settingsFixture(t)
	ctx := t.Context()
	conv, err := s.CreateConversation(ctx, "manual", domain.ConversationScopeSession, rec.ProjectID, rec.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.BeginAgentSettingsOperation(ctx, settingsOperation(rec.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitAgentSettingsOperation(ctx, rec.ID, op.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConversationManualControls(ctx, conv.ID, domain.ConversationSettings{Model: "old", ReasoningEffort: "old", AgentFastMode: domain.AgentFastOn, ApprovalMode: domain.PermissionModeAuto, AutoCompactWindow: "200k"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	saved, err := s.LoadConversationSnapshot(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Conversation.Settings.Model != string(op.Target.Model) || saved.Conversation.Settings.ReasoningEffort != string(op.Target.Effort) || saved.Conversation.Settings.AgentFastMode != op.Target.FastMode || saved.Conversation.Settings.ApprovalMode != domain.PermissionModeAuto || saved.Conversation.Settings.AutoCompactWindow != "200k" {
		t.Fatalf("controls overwrote selection: %+v", saved.Conversation.Settings)
	}
}
