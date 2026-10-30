package chat_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// The SQL permission boundary is covered by the replacement service's real
// transfer fixture. Here only that permission/source history and provider are
// doubles; canonical queue writes, claims, binding and settlement remain real.
type relocatedExcerptQueueStore struct {
	chatsvc.Store
	hash, generation, target string
}

func (s *relocatedExcerptQueueStore) ReplacementExcerptSourceAllowed(_ context.Context, session domain.SessionID, generation, target, turn, source, hash string) (bool, error) {
	return session == testSession && generation == s.generation && target == s.target && turn == "relocated-turn" && source == "retained-source" && hash == s.hash, nil
}
func (s *relocatedExcerptQueueStore) ConversationMessages(ctx context.Context, id string) ([]domain.ConversationMessage, error) {
	if id == "retained-source" {
		return []domain.ConversationMessage{{ID: "retained-user", ConversationID: id, TurnID: "retained-completed", Role: domain.MessageRoleUser, Text: "original source question", Revision: 1}, {ID: "retained-answer", ConversationID: id, TurnID: "retained-completed", Role: domain.MessageRoleAssistant, Text: "retained source selected answer", Revision: 1}}, nil
	}
	return s.Store.ConversationMessages(ctx, id)
}
func (s *relocatedExcerptQueueStore) TurnByID(ctx context.Context, id string) (domain.ConversationTurn, error) {
	if id == "retained-completed" {
		return domain.ConversationTurn{ID: id, ConversationID: "retained-source", State: domain.TurnStateCompleted}, nil
	}
	return s.Store.TurnByID(ctx, id)
}
func TestReplacementRelocatedExcerptDrainsCanonicalQueueOnce(t *testing.T) {
	var wrapped *relocatedExcerptQueueStore
	h := newHarnessWithConversationAndStoreForHarness(t, nil, func(st *sqlite.Store) chatsvc.Store { wrapped = &relocatedExcerptQueueStore{Store: st}; return wrapped }, domain.HarnessClaudeCode)
	ctx := context.Background()
	active, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "active before intake", ClientMessageID: "active", Origin: domain.MessageOriginHuman})
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := h.st.GetSession(ctx, testSession)
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal([]ports.ChatContent{{Type: "image", Data: "aW1hZ2U=", MIMEType: "image/png"}, {Type: "excerpt", Excerpt: &ports.ChatExcerptContext{Reference: ports.ChatExcerptReference{ConversationID: "retained-source", MessageID: "retained-answer", Revision: 1, Text: "selected answer"}}}})
	if err != nil {
		t.Fatal(err)
	}
	queued := domain.QueuedTurn{TurnID: "relocated-turn", ClientMessageID: "original-source-client", Text: "A-Q28: B; preserve selected context", Origin: domain.MessageOriginHuman, DeliveryContentJSON: string(content)}
	wrapped.hash, err = domain.ReplacementIntakePayloadHash(queued)
	if err != nil {
		t.Fatal(err)
	}
	wrapped.generation, wrapped.target = target.Metadata.ControllerGeneration, h.ctrl.ConversationID()
	if added, err := h.st.AppendUserMessage(ctx, wrapped.target, testSession, wrapped.generation, domain.ConversationMessage{ID: "original-source-message", ClientMessageID: queued.ClientMessageID, Text: queued.Text, Origin: queued.Origin, DeliveryContentJSON: queued.DeliveryContentJSON}, queued.TurnID, time.Now()); err != nil || !added {
		t.Fatal(added, err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: active.ProviderTurnID, TurnState: domain.TurnStateCompleted})
	h.awaitSnapshot(t, func(snapshot store.ConversationSnapshot) bool {
		for _, turn := range snapshot.Turns {
			if turn.ID == queued.TurnID {
				return turn.State == domain.TurnStateRunning && turn.ProviderTurnID != ""
			}
		}
		return false
	})
	sent := h.conv.sentMessages()
	if len(sent) != 2 || !strings.Contains(sent[1].Text, "original source question") || !strings.Contains(sent[1].Text, "retained source selected answer") || len(sent[1].Content) != 1 || sent[1].Content[0].Type != "image" || sent[1].Content[0].Data != "aW1hZ2U=" {
		t.Fatal("native drain lost relocated payload/context", sent)
	}
	turn, err := h.st.TurnByID(ctx, queued.TurnID)
	if err != nil || turn.ProviderTurnID == "" {
		t.Fatal("original turn lost provider binding", turn, err)
	}
	h.conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateCompleted})
	h.awaitSnapshot(t, func(snapshot store.ConversationSnapshot) bool {
		for _, turn := range snapshot.Turns {
			if turn.ID == queued.TurnID {
				return turn.State == domain.TurnStateCompleted
			}
		}
		return false
	})
	if err := h.svc.DrainQueued(ctx, testSession); err != nil {
		t.Fatal(err)
	}
	if len(h.conv.sentMessages()) != 2 {
		t.Fatal("completed relocated input dispatched twice")
	}
	message, found, err := h.st.ConversationMessageByClientID(ctx, wrapped.target, queued.ClientMessageID)
	if err != nil || !found || message.ID != "original-source-message" || message.DeliveryContentJSON != string(content) {
		t.Fatal("drain rewrote original canonical payload", found, err)
	}
}
