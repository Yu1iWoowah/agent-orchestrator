package chat_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func archiveNativeTurnIdentity(t *testing.T, st *sqlite.Store, conversationID, generation, turnID, threadID string, at time.Time) {
	t.Helper()
	payload := fmt.Sprintf(`{"providerTurnId":%q,"providerConversationId":%q}`, turnID, threadID)
	if _, err := st.ProjectProviderEvent(context.Background(), conversationID, testSession, generation, "identity-"+turnID, "turn.started", payload, at, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestNativeThreadCheckpointRetainsForkAncestorHighWater(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing_ancestor_%v", missing), func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			rec, _, err := h.st.GetSession(ctx, testSession)
			if err != nil {
				t.Fatal(err)
			}
			rec.Metadata.ProviderConversationID = h.conv.ProviderConversationID()
			if err := h.st.UpdateSession(ctx, rec); err != nil {
				t.Fatal(err)
			}
			turnID := completeTurn(t, h, "ancestor", "provider-turn-1")
			before := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				return len(s.Turns) == 1 && s.Turns[0].State == domain.TurnStateCompleted
			})
			if err := h.svc.Stop(ctx, testSession); err != nil {
				t.Fatal(err)
			}
			branch := domain.ConversationBranch{
				ID: "native-fork", ConversationID: before.Conversation.ID, SessionID: testSession,
				ProviderConversationID: "fork-thread", ParentBranchID: before.ActiveBranch.ID,
				ForkAfterTurnID: turnID, ForkAfterSequence: before.Conversation.LatestSequence,
				ProviderScopeID: before.ActiveBranch.ProviderScopeID,
			}
			if err := h.st.CreateAndActivateConversationBranch(ctx, testSession, branch, "fork-generation", h.now()); err != nil {
				t.Fatal(err)
			}
			events := []ports.ChatEvent{
				{Kind: ports.ChatEventUserMessageCompleted, ProviderEventID: "fork-user", ProviderTurnID: "provider-turn-1", Text: "ancestor"},
				{Kind: ports.ChatEventMessageCompleted, ProviderEventID: "fork-answer", ProviderTurnID: "provider-turn-1", ProviderItemID: "msg-provider-turn-1", Text: "reply to ancestor"},
				{Kind: ports.ChatEventTurnCompleted, ProviderEventID: "fork-completed", ProviderTurnID: "provider-turn-1", ProviderConversationID: "fork-thread", TurnState: domain.TurnStateCompleted},
			}
			if missing {
				events = nil
			}
			conv := &nativeHistoryConversation{fakeConversation: newFakeConversation(), events: events}
			conv.providerConversationID = "fork-thread"
			svc := chatsvc.New(chatsvc.Options{Store: h.st, Sessions: h.st, Reader: fullSnapshotReader(h.st), Drivers: fakeRegistry{driver: fakeDriver{conv: conv}}, NewID: uuid.NewString})
			t.Cleanup(func() { svc.StopAll(ctx) })
			resumeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			_, err = svc.Start(resumeCtx, chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, ProviderConversationID: "fork-thread", HistoryMode: ports.ChatHistoryRequired})
			if missing && !errors.Is(err, ports.ErrChatHistoryUnsettled) {
				t.Fatalf("fork admitted missing ancestor: %v", err)
			}
			if !missing && err != nil {
				t.Fatalf("fork rejected complete ancestor: %v", err)
			}
		})
	}
}

func TestNativeHistoryCheckpointUsesNativeThreadProvenance(t *testing.T) {
	for _, test := range []struct {
		name                       string
		childFinishesLast          bool
		unknown, conflict, missing bool
		includeChildReplay         bool
		providerHistory            bool
		tiedRequests               bool
		wantErr                    bool
	}{
		{name: "legacy archive identifies child"},
		{name: "child completes after parent", childFinishesLast: true},
		{name: "child and parent have tied request times", tiedRequests: true},
		{name: "missing parent still blocks", missing: true, wantErr: true},
		{name: "child replay cannot replace missing parent", missing: true, includeChildReplay: true, wantErr: true},
		{name: "unknown ownership even with matching replay", unknown: true, includeChildReplay: true, wantErr: true},
		{name: "provider history cannot waive unknown ownership", unknown: true, includeChildReplay: true, providerHistory: true, wantErr: true},
		{name: "conflicting ownership even with matching replay", conflict: true, includeChildReplay: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{
				Text: "root prompt", ClientMessageID: "root-client", Origin: domain.MessageOriginHuman,
			}); err != nil {
				t.Fatal(err)
			}
			h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1", ProviderConversationID: "thread-1"})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				return len(s.Turns) == 1 && s.Turns[0].ProviderTurnID == "provider-turn-1"
			})
			if !test.tiedRequests {
				h.advance(time.Second)
			}
			childThread, completedThread := "child-thread", "child-thread"
			if test.unknown {
				childThread, completedThread = "", ""
			}
			if test.conflict {
				completedThread = "thread-1"
			}
			h.conv.emit(
				ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "child-turn", ProviderConversationID: childThread},
				ports.ChatEvent{Kind: ports.ChatEventMessageCompleted, ProviderTurnID: "child-turn", ProviderItemID: "child-answer", Text: "child result"},
			)
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) == 2 })
			completeChild := ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "child-turn", ProviderConversationID: completedThread, TurnState: domain.TurnStateCompleted}
			completeRoot := ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "provider-turn-1", ProviderConversationID: "thread-1", TurnState: domain.TurnStateCompleted}
			h.advance(time.Second)
			rootAnswer := ports.ChatEvent{Kind: ports.ChatEventMessageCompleted, ProviderTurnID: "provider-turn-1", ProviderItemID: "root-answer", Text: "root result"}
			if test.childFinishesLast {
				h.conv.emit(rootAnswer, completeRoot, completeChild)
			} else {
				h.conv.emit(completeChild, rootAnswer, completeRoot)
			}
			before := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				return len(s.Turns) == 2 && s.Turns[0].State == domain.TurnStateCompleted && s.Turns[1].State == domain.TurnStateCompleted
			})
			if err := h.svc.Stop(ctx, testSession); err != nil {
				t.Fatal(err)
			}
			events := []ports.ChatEvent{
				{Kind: ports.ChatEventUserMessageCompleted, ProviderTurnID: "provider-turn-1", ProviderItemID: "root-user", ClientMessageID: "root-client", Text: "root prompt"},
				rootAnswer, completeRoot,
			}
			if test.missing {
				events = nil
			}
			if test.includeChildReplay {
				events = append(events, ports.ChatEvent{Kind: ports.ChatEventMessageCompleted, ProviderTurnID: "child-turn", ProviderItemID: "child-answer", Text: "child result"}, completeChild)
			}
			for i := range events {
				events[i].ProviderEventID = fmt.Sprintf("replay-%d", i)
			}
			conv := &nativeHistoryConversation{fakeConversation: newFakeConversation(), events: events}
			svc := chatsvc.New(chatsvc.Options{Store: h.st, Sessions: h.st, Reader: fullSnapshotReader(h.st), Drivers: fakeRegistry{driver: fakeDriver{conv: conv}}, NewID: uuid.NewString})
			t.Cleanup(func() { svc.StopAll(context.Background()) })
			resumeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			policy := domain.SessionInterfaceTransitionHistoryStrict
			if test.providerHistory {
				policy = domain.SessionInterfaceTransitionHistoryProvider
			}
			_, err := svc.Start(resumeCtx, chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex, ProviderConversationID: "thread-1", HistoryMode: ports.ChatHistoryRequired, HistoryPolicy: policy})
			if test.wantErr {
				if !errors.Is(err, ports.ErrChatHistoryUnsettled) {
					t.Fatalf("resume = %v, want history rejected", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("complete root history rejected because of child timeline: %v", err)
			}
			after, err := h.st.LoadConversationSnapshot(ctx, before.Conversation.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range after.Messages {
				if message.ProviderItemID == "child-answer" && message.Text == "child result" {
					return
				}
			}
			t.Fatal("child timeline was discarded by root replay")
		})
	}
}
