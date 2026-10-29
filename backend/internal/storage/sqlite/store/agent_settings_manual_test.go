package store_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

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
	if err := s.SetConversationManualControls(ctx, conv.ID, domain.ConversationSettings{Model: "old", ReasoningEffort: "old", AgentFastMode: domain.AgentFastOn, ApprovalMode: domain.PermissionModeAuto, AutoCompactWindow: "200k"}); err != nil {
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
