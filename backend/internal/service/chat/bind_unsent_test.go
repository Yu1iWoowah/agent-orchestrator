package chat_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Preparing a deferred prompt must not count as sending it to the provider.
type bindDeferredConversation struct {
	*fakeConversation
	prepared ports.ChatUserMessage
	starts   atomic.Int32
	discards atomic.Int32
}

func (c *bindDeferredConversation) SendTurn(_ context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.turnSeq++
	c.prepared = msg
	return ports.ChatTurnRef{ProviderTurnID: fmt.Sprintf("prepared-%d", c.turnSeq)}, nil
}
func (c *bindDeferredConversation) StartDeferredTurn(id string) error {
	c.mu.Lock()
	c.sent = append(c.sent, c.prepared)
	c.prepared = ports.ChatUserMessage{}
	c.mu.Unlock()
	c.starts.Add(1)
	c.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: id})
	return nil
}
func (c *bindDeferredConversation) DiscardDeferredTurn(string) {
	c.mu.Lock()
	c.prepared = ports.ChatUserMessage{}
	c.mu.Unlock()
	c.discards.Add(1)
}

type bindUnsentFaultStore struct {
	chatsvc.Store
	armed atomic.Bool
}

var errBindUnsent = errors.New("bind-unsent-injected")

func (s *bindUnsentFaultStore) BindTurnToProvider(ctx context.Context, id, pid string, at time.Time) error {
	if s.armed.CompareAndSwap(true, false) {
		return errBindUnsent
	}
	return s.Store.BindTurnToProvider(ctx, id, pid, at)
}

func TestDeferredBindFailurePreservesUnsentInput(t *testing.T) {
	for _, mode := range []string{"direct", "queued", "batch"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			conv := &bindDeferredConversation{fakeConversation: newFakeConversation()}
			fault := &bindUnsentFaultStore{}
			h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store {
				fault.Store = db
				return fault
			})
			if mode != "direct" {
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "previous", ProviderInitiated: true})
				h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) == 1 })
			}
			fault.armed.Store(true)
			texts := []string{"keep lead"}
			if mode == "batch" {
				texts = append(texts, "keep follower")
			}
			for _, text := range texts {
				_, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: text, ClientMessageID: text})
				if mode == "direct" && !errors.Is(err, errBindUnsent) || mode != "direct" && err != nil {
					t.Fatalf("Send: %v", err)
				}
			}
			if mode != "direct" {
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "previous", TurnState: domain.TurnStateCompleted})
			}
			// An event behind completion fences the entire failed dispatch and cleanup.
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "previous", ProviderItemID: "bind-fence", ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: "bind-fence"})
			rows := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				for _, a := range s.Activities {
					if a.Summary == "bind-fence" {
						return true
					}
				}
				return false
			})
			for _, msg := range rows.Messages {
				if msg.ClientMessageID == "" {
					continue
				}
				turn, err := h.st.TurnByID(ctx, msg.TurnID)
				if err != nil || turn.State != domain.TurnStateQueued || turn.AgentSettings != nil || turn.ProviderInputText != "" || turn.ProviderTurnID != "" || turn.StartedAt != nil {
					t.Fatalf("Bind failure stranded unsent input: state=%s snapshot=%+v input=%q provider=%q started=%v err=%v", turn.State, turn.AgentSettings, turn.ProviderInputText, turn.ProviderTurnID, turn.StartedAt, err)
				}
			}
			if conv.starts.Load() != 0 || conv.discards.Load() != 1 {
				t.Fatalf("failed binding crossed provider boundary: starts=%d discards=%d", conv.starts.Load(), conv.discards.Load())
			}
			h.advance(time.Second)
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady})
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "previous", ProviderItemID: "ready-fence", ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: "ready-fence"})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				for _, activity := range s.Activities {
					if activity.Summary == "ready-fence" {
						return true
					}
				}
				return false
			})
			messages := conv.sentMessages()
			if len(messages) != 1 {
				t.Fatalf("input lost or duplicated: %+v", messages)
			}
			if mode == "batch" {
				items := parseDeliveredBatch(t, messages[0].Text)
				if len(items) != 2 || items[0].Text != texts[0] || items[1].Text != texts[1] {
					t.Fatalf("batch input changed: %+v", items)
				}
			} else if messages[0].Text != texts[0] {
				t.Fatalf("input changed: %+v", messages)
			}
			var retries sync.WaitGroup
			for range 4 {
				retries.Go(func() {
					for _, text := range texts {
						if _, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: text, ClientMessageID: text}); err != nil {
							t.Errorf("same-ID retry: %v", err)
						}
					}
				})
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady})
			}
			retries.Wait()
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "previous", ProviderItemID: "retry-fence", ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: "retry-fence"})
			rows = h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				for _, a := range s.Activities {
					if a.Summary == "retry-fence" {
						return true
					}
				}
				return false
			})
			if conv.starts.Load() != 1 || len(conv.sentMessages()) != 1 {
				t.Fatalf("Ready/same-ID retries duplicated delivery: starts=%d messages=%+v", conv.starts.Load(), conv.sentMessages())
			}
			for _, msg := range rows.Messages {
				if msg.ClientMessageID == "" {
					continue
				}
				saved, err := h.st.TurnByID(ctx, msg.TurnID)
				if err != nil || saved.State != domain.TurnStateRunning && saved.State != domain.TurnStateCompleted || saved.AgentSettings == nil {
					t.Fatalf("retry lost durable input: %+v %v", saved, err)
				}
			}
		})
	}
}
