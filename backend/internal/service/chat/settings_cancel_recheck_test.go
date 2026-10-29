package chat_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type settingsBlockingCompact struct {
	*journalConversation
	entered, release chan struct{}
}

func (*settingsBlockingCompact) Capabilities() ports.ChatCapabilities {
	caps := productionCaps()
	caps[ports.ChatCapabilityCompaction] = true
	return caps
}

func (c *settingsBlockingCompact) Compact(context.Context) (ports.ChatCompactionResult, error) {
	close(c.entered)
	<-c.release
	return ports.ChatCompactionResult{}, errors.New("fake compaction declined")
}

func TestSettingsQueuedWhileIdleLockHeldRunsAfterUnlock(t *testing.T) {
	conv := &journalConversation{fakeConversation: newFakeConversation()}
	conv.quiet.Store(true)
	blocked := &settingsBlockingCompact{journalConversation: conv, entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(blocked.release) })
	h := newHarnessWithConversationAndStoreForHarness(t, blocked, func(st *sqlite.Store) chatsvc.Store {
		if err := st.BindSessionAgentAccount(t.Context(), testSession, domain.AgentAccountProviderCodex, "device-codex", time.Now()); err != nil {
			t.Fatal(err)
		}
		return st
	}, domain.HarnessCodex)
	rec, _, err := h.st.GetSession(t.Context(), testSession)
	if err != nil {
		t.Fatal(err)
	}
	rec.Metadata.ProviderConversationID = conv.ProviderConversationID()
	if err := h.st.UpdateSession(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	h.svc.SetAgentSettingsValidator(&journalValidator{})
	finished := make(chan struct{})
	go func() { _, _ = h.ctrl.Compact(t.Context()); close(finished) }()
	select {
	case <-blocked.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("idle lock holder did not enter")
	}
	op, err := h.ctrl.QueueAgentSettings(t.Context(), journalIntent(t, h))
	if err != nil || op.Phase != domain.AgentSettingsPending {
		t.Fatalf("idle intent not accepted: %+v %v", op, err)
	}
	release.Do(func() { close(blocked.release) })
	<-finished
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		saved, err := h.st.AgentSettingsOperation(t.Context(), testSession, op.ID)
		return err == nil && saved.Phase == domain.AgentSettingsApplied
	})
	if len(conv.sentMessages()) != 0 {
		t.Fatal("settings recheck manufactured a user turn")
	}
}

type settingsBlockingPrepare struct {
	*journalConversation
	entered, release chan struct{}
	once             sync.Once
}

func (c *settingsBlockingPrepare) PrepareAgentSettings(ctx context.Context, s, t domain.AgentSettings) (domain.AgentSettingsProviderPlan, error) {
	c.once.Do(func() { close(c.entered); <-c.release })
	return c.journalConversation.PrepareAgentSettings(ctx, s, t)
}
func TestSettingsCancelWhilePreparingWakesExistingQueue(t *testing.T) {
	conv := &journalConversation{fakeConversation: newFakeConversation()}
	conv.quiet.Store(true)
	blocked := &settingsBlockingPrepare{journalConversation: conv, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(blocked.release) })
	h := newHarnessWithConversationAndStoreForHarness(t, blocked, func(st *sqlite.Store) chatsvc.Store {
		if err := st.BindSessionAgentAccount(t.Context(), testSession, domain.AgentAccountProviderCodex, "device-codex", time.Now()); err != nil {
			t.Fatal(err)
		}
		return st
	}, domain.HarnessCodex)
	rec, _, _ := h.st.GetSession(t.Context(), testSession)
	rec.Metadata.ProviderConversationID = conv.ProviderConversationID()
	if err := h.st.UpdateSession(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	h.svc.SetAgentSettingsValidator(&journalValidator{})
	first, err := h.ctrl.Send(t.Context(), ports.ChatUserMessage{Text: "first"})
	if err != nil {
		t.Fatal(err)
	}
	op, err := h.ctrl.QueueAgentSettings(t.Context(), journalIntent(t, h))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.ctrl.Send(t.Context(), ports.ChatUserMessage{Text: "already queued"}); err != nil {
		t.Fatal(err)
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: first.ProviderTurnID, TurnState: domain.TurnStateCompleted})
	select {
	case <-blocked.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("prepare not entered")
	}
	cancelled, err := h.ctrl.CancelAgentSettings(t.Context(), op.ID, op.Version)
	if err != nil || cancelled.Phase != domain.AgentSettingsCancelled {
		t.Fatalf("cancel %+v %v", cancelled, err)
	}
	releaseOnce.Do(func() { close(blocked.release) })
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(conv.sentMessages()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(conv.sentMessages()) != 2 {
		snap, _ := h.st.LoadConversationSnapshot(t.Context(), h.ctrl.ConversationID())
		t.Fatalf("cancelled pending left existing queue stranded: provider sends=%d turns=%+v", len(conv.sentMessages()), snap.Turns)
	}
}
