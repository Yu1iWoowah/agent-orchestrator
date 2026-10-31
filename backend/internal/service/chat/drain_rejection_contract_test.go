package chat_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type invalidDrainMemberStore struct{ *store.Store }

func (s *invalidDrainMemberStore) ListQueuedBatch(ctx context.Context, id string, limit int) ([]domain.QueuedTurn, error) {
	batch, err := s.Store.ListQueuedBatch(ctx, id, limit)
	for i := range batch {
		if batch[i].ClientMessageID == "invalid" {
			batch[i].DeliveryContentJSON = "{"
		}
	}
	return batch, err
}

func TestHandledQueueRejectionsDoNotFailLifecycleOperations(t *testing.T) {
	for _, mode := range []string{"drainOne", "handoff", "applySettings", "cancelSettings", "stop"} {
		t.Run(mode, func(t *testing.T) {
			h, conv, _, commands := journalHarnessWithStore(t, func(db *store.Store) chatsvc.Store { return &invalidDrainMemberStore{Store: db} })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var op domain.AgentSettingsOperation
			if mode == "applySettings" || mode == "cancelSettings" {
				conv.quiet.Store(false)
				var err error
				op, err = commands.QueueAgentSettings(ctx, journalIntent(t, h))
				if err != nil || op.Phase != domain.AgentSettingsPending {
					t.Fatalf("queue settings: %+v %v", op, err)
				}
			}
			appendTurn := func(id string, at time.Time) {
				t.Helper()
				created, err := h.st.AppendUserMessage(ctx, h.ctrl.ConversationID(), testSession, h.ctrl.Generation(), domain.ConversationMessage{ID: id + "-message", ClientMessageID: id, Text: id, Origin: domain.MessageOriginHuman}, id+"-turn", at)
				if err != nil || !created {
					t.Fatalf("append %s: %t %v", id, created, err)
				}
			}
			if mode == "stop" {
				appendTurn("orphan", h.now())
				if err := h.st.BindTurnToProvider(ctx, "orphan-turn", "orphan-provider", h.now()); err != nil {
					t.Fatal(err)
				}
			}
			at := h.now()
			if mode == "stop" {
				at = at.Add(time.Second)
			}
			for _, id := range []string{"invalid", "valid-one", "valid-two"} {
				appendTurn(id, at)
			}
			var operationErr error
			switch mode {
			case "drainOne":
				operationErr = h.svc.DrainQueued(ctx, testSession)
			case "handoff":
				conv.onSend = func(id string) {
					conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: id, TurnState: domain.TurnStateCompleted})
				}
				operationErr = h.svc.PrepareChatHandoff(ctx, testSession, domain.SessionInterfaceTransitionDrain)
			case "applySettings":
				conv.quiet.Store(true)
				operationErr = commands.RunAgentSettings(ctx)
				saved, err := h.st.AgentSettingsOperation(ctx, testSession, op.ID)
				if err != nil || saved.Phase != domain.AgentSettingsApplied {
					t.Fatalf("settings not applied: %+v %v", saved, err)
				}
			case "cancelSettings":
				saved, err := h.ctrl.CancelAgentSettings(ctx, op.ID, op.Version)
				operationErr = err
				if saved.Phase != domain.AgentSettingsCancelled {
					t.Fatalf("settings not cancelled: %+v", saved)
				}
			case "stop":
				operationErr = h.svc.Interrupt(ctx, testSession)
				orphan, err := h.st.TurnByID(ctx, "orphan-turn")
				if err != nil || orphan.State != domain.TurnStateInterrupted {
					t.Fatalf("Stop did not reconcile: %+v %v", orphan, err)
				}
			}
			if operationErr != nil {
				t.Errorf("handled invalid member failed %s: %v", mode, operationErr)
			}
			bad, err := h.st.TurnByID(ctx, "invalid-turn")
			if err != nil || bad.State != domain.TurnStateFailed {
				t.Fatalf("invalid member not settled: %+v %v", bad, err)
			}
			first, err := h.st.TurnByID(ctx, "valid-one-turn")
			if err != nil || first.ProviderTurnID == "" || first.State == domain.TurnStateFailed {
				t.Fatalf("valid input lost: %+v %v", first, err)
			}
			last, err := h.st.TurnByID(ctx, "valid-two-turn")
			if err != nil || last.State == domain.TurnStateFailed {
				t.Fatalf("later valid input lost: %+v %v", last, err)
			}
			if mode == "drainOne" {
				if last.State != domain.TurnStateQueued {
					t.Fatalf("drainOne consumed later input: %+v", last)
				}
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: first.ProviderTurnID, TurnState: domain.TurnStateCompleted})
				h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
					row, err := h.st.TurnByID(context.Background(), last.ID)
					return err == nil && row.ProviderTurnID != "" && row.State == domain.TurnStateRunning
				})
			}
			if mode == "handoff" {
				if _, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "must remain fenced", ClientMessageID: "late"}); !errors.Is(err, chatsvc.ErrControllerHandoff) {
					t.Errorf("handled rejection aborted handoff: %v", err)
				}
			}
		})
	}
}
