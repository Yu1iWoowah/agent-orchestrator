package chat_test

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestSettingsServiceSendDoesNotHoldProviderWorkWhileQueued(t *testing.T) {
	h, conv, _, commands := journalHarness(t)
	conv.quiet.Store(false)
	op, err := commands.QueueAgentSettings(t.Context(), journalIntent(t, h))
	if err != nil || op.Phase != domain.AgentSettingsPending {
		t.Fatalf("pending intent: %+v %v", op, err)
	}
	conv.quiet.Store(true)
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "accepted behind settings", ClientMessageID: "settings-intake"})
	if err != nil || turn.State != domain.TurnStateQueued {
		t.Fatalf("durable intake: %+v %v", turn, err)
	}
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		saved, err := h.st.AgentSettingsOperation(t.Context(), testSession, op.ID)
		return err == nil && saved.Phase == domain.AgentSettingsApplied && len(conv.sentMessages()) == 1
	})
	if texts := conv.sentTexts(); len(texts) != 1 || texts[0] != "accepted behind settings" {
		t.Fatalf("provider dispatches: %v", texts)
	}
}

func TestSettingsColdRefreshRecoversAfterOneServiceSend(t *testing.T) {
	h, _, validator, commands := journalHarness(t)
	if op, err := commands.QueueAgentSettings(t.Context(), journalIntent(t, h)); err != nil || op.Phase != domain.AgentSettingsApplied {
		t.Fatalf("apply: %+v %v", op, err)
	}
	if err := h.svc.Stop(t.Context(), testSession); err != nil {
		t.Fatal(err)
	}
	conv := &journalConversation{fakeConversation: newFakeConversation()}
	conv.quiet.Store(true)
	conv.freshRecovery.Store(true)
	conv.prepareError.Store(true)
	var ids atomic.Int64
	svc := chatsvc.New(chatsvc.Options{
		Store: h.st, Sessions: h.st, Reader: fullSnapshotReader(h.st), PageReader: boundedSnapshotReader(h.st),
		Drivers: fakeRegistry{driver: fakeDriver{conv: conv}}, Activity: h.activity,
		Log: slog.New(slog.DiscardHandler), Now: h.now,
		NewID: func() string { return fmt.Sprintf("intake-recovery-%d", ids.Add(1)) },
	})
	svc.SetAgentSettingsValidator(validator)
	_, err := svc.Start(t.Context(), chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, WorkspacePath: t.TempDir(), ProviderConversationID: conv.ProviderConversationID()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background(), testSession) })
	conv.prepareError.Store(false)
	turn, err := svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "recover without a second send", ClientMessageID: "cold-intake"})
	if err != nil || turn.State != domain.TurnStateQueued {
		t.Fatalf("durable intake: %+v %v", turn, err)
	}
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool { return len(conv.sentMessages()) == 1 })
	if got := conv.stepCount(); got != 4 {
		t.Fatalf("cold reconciliation steps: %d", got)
	}
	if texts := conv.sentTexts(); len(texts) != 1 || texts[0] != "recover without a second send" {
		t.Fatalf("provider dispatches: %v", texts)
	}
}
