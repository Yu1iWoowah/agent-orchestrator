package chat_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestDispatchSnapshotIsDurableBeforeProviderAndFrozenAfterSettingsChange(t *testing.T) {
	ctx := context.Background()
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		if err := st.BindSessionAgentAccount(ctx, testSession, domain.AgentAccountProviderCodex, "device-codex", time.Now()); err != nil {
			t.Fatal(err)
		}
		return st
	})
	record, _, err := h.st.GetSession(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	record.Metadata.ProviderConversationID = h.ctrl.ProviderConversationID()
	if err := h.st.UpdateSession(ctx, record); err != nil {
		t.Fatal(err)
	}
	var atProvider *domain.AgentSettingsTurnSnapshot
	h.conv.onSend = func(string) {
		rows, err := h.st.LoadConversationSnapshot(ctx, h.ctrl.ConversationID())
		if err != nil {
			t.Error(err)
			return
		}
		if len(rows.Turns) == 1 {
			atProvider = rows.Turns[0].AgentSettings
		}
	}
	turn, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "unchanged prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if atProvider == nil || atProvider.Revision == nil || *atProvider.Revision != 0 || atProvider.Settings == nil || atProvider.Source != "root" || atProvider.Owner == nil || atProvider.Owner.ControllerGeneration != h.ctrl.Generation() {
		t.Fatalf("provider received turn before qualified durable snapshot: %+v", atProvider)
	}
	before := *atProvider.Settings
	if err := h.ctrl.SetSettings(ctx, domain.ConversationSettings{Model: "later"}); err != nil {
		t.Fatal(err)
	}
	saved, err := h.st.TurnByID(ctx, turn.ID)
	if err != nil || saved.AgentSettings == nil || saved.AgentSettings.Settings == nil || *saved.AgentSettings.Settings != before || saved.AgentSettings.ProviderTurnID != "provider-turn-1" {
		t.Fatalf("later preference rewrote historical wire: %+v %v", saved.AgentSettings, err)
	}
}

type snapshotFailureStore struct{ chatsvc.Store }

func (s *snapshotFailureStore) RecordTurnSettingsSnapshot(context.Context, string, string, []string, domain.AgentSettingsTurnSnapshot) error {
	return errors.New("dispatch-snapshot-injected")
}

func TestDispatchSnapshotFailureDoesNotReachProviderOrLosePrompt(t *testing.T) {
	h := newHarnessWithConversationAndStore(t, nil, func(st *sqlite.Store) chatsvc.Store {
		return &snapshotFailureStore{Store: st}
	})
	receipt, err := h.ctrl.Send(context.Background(), ports.ChatUserMessage{Text: "keep my prompt"})
	if err != nil || receipt.State != domain.TurnStateQueued {
		t.Fatalf("snapshot failure did not preserve accepted input: %+v %v", receipt, err)
	}
	if texts := h.conv.sentTexts(); len(texts) != 0 {
		t.Fatalf("provider received unrecorded wire: %v", texts)
	}
	rows := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Messages) == 1 })
	if rows.Messages[0].Text != "keep my prompt" || len(rows.Turns) != 1 || rows.Turns[0].State != domain.TurnStateQueued {
		t.Fatalf("snapshot failure lost prompt/settlement: %+v", rows)
	}
}
