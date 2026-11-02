package chat

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type recoveredSideCloseConversation struct {
	compactSideTestConversation
	closed bool
	events chan ports.ChatEvent
}

func (c *recoveredSideCloseConversation) Close() error                   { c.closed = true; return nil }
func (c *recoveredSideCloseConversation) Events() <-chan ports.ChatEvent { return c.events }
func (c *recoveredSideCloseConversation) QueryDeliveryAttempt(context.Context, ports.ChatDeliveryLookup) (domain.ChatDeliveryReceipt, error) {
	return domain.ChatDeliveryReceipt{}, ports.ErrChatDeliveryUnverified
}
func (c *recoveredSideCloseConversation) FenceAndFlushOutbound(context.Context, []ports.ChatDeliveryLookup) ([]domain.ChatDeliveryReceipt, error) {
	return nil, nil
}
func TestSideUnverifiedCompactionSurvivesRecovery(t *testing.T) {
	ctx := context.Background()
	at := time.Now().UTC()
	before := New(Options{AppRunID: "audit-side", Sessions: sideSessionReader{}})
	defer before.sides.cancel()
	_, _ = before.sides.store.ClaimSideLaunch(ctx, "audit-side", at)
	side := domain.SideConversation{PolicyVersion: 1, ID: "side", SessionID: "session", MainConversationID: "main", AppRunID: "audit-side", ProviderHostID: "btw-audit-side-side", ProviderForkID: "provider-side-1", State: "opening", Generation: "g"}
	if _, _, err := before.sides.store.CreateSideConversation(ctx, side); err != nil {
		t.Fatal(err)
	}
	if err := before.sides.store.SetSideReady(ctx, side.ID, side.Generation, side.ProviderForkID, at); err != nil {
		t.Fatal(err)
	}
	provider := &compactSideTestConversation{}
	before.sides.runtimes[side.ID] = &sideRuntime{side: side, conv: provider}
	if _, err := before.CompactSideChat(ctx, side.SessionID, side.ID); err != nil {
		t.Fatal(err)
	}
	if err := before.PreflightShutdown(ctx); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatalf("unverified compact must block before recovery: %v", err)
	}
	records, err := before.ExportSideChatLaunch(ctx, "audit-side")
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	var restored []SideRecoveryRecord
	if err := json.Unmarshal(wire, &restored); err != nil {
		t.Fatal(err)
	}
	after := New(Options{AppRunID: "audit-side", Sessions: sideSessionReader{}})
	defer after.sides.stopAll()
	_, _ = after.sides.store.ClaimSideLaunch(ctx, "audit-side", at)
	recovered, err := after.sides.store.(*memorySideStore).recover("audit-side", restored, at)
	if err != nil || len(recovered) != 1 {
		t.Fatalf("recover %v %+v", err, recovered)
	}
	if err := after.StopAll(ctx); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatalf("unattached recovered side disappeared from coverage: %v", err)
	}
	reconnected := &recoveredSideCloseConversation{events: make(chan ports.ChatEvent)}
	if !after.sides.attach(recovered[0], reconnected) {
		t.Fatal("attach failed")
	}
	if err := after.StopAll(ctx); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatalf("side compaction gap lost across JSON recovery: StopAll=%v transportClosed=%v", err, reconnected.closed)
	}
	if restored[0].CompactionID == "" {
		t.Fatal("recovery lost original command correlation")
	}
	// Historical replay and unrelated live/background completions prove nothing
	// about this command. The unbuffered marker orders both before the check.
	for _, event := range []ports.ChatEvent{
		{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "original-compaction", TurnState: domain.TurnStateCompleted},
		{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "background-turn", ProviderEventID: "background-receipt", CompactionID: "different-command", TurnState: domain.TurnStateCompleted},
		{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "marker", TurnState: domain.TurnStateCompleted},
	} {
		select {
		case reconnected.events <- event:
		case <-time.After(2 * time.Second):
			t.Fatal("side event consumer stopped")
		}
	}
	if err := after.PreflightShutdown(ctx); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatalf("unrelated completion removed command fence: %v", err)
	}
	// An empty custody flush is no execution receipt. Only the original native
	// completion can retire this gap, including after recovery lost compacting.
	reconnected.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, CompactionID: restored[0].CompactionID, ProviderTurnID: "original-compaction", TurnState: domain.TurnStateCompleted}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := after.PreflightShutdown(ctx); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := after.StopAll(ctx); err != nil {
		t.Fatalf("native completion did not settle recovered side: %v", err)
	}
	if !reconnected.closed {
		t.Fatal("settled side did not close")
	}
}

type failedCompactRetryConversation struct {
	recoveredSideCloseConversation
	ids []string
}

func (c *failedCompactRetryConversation) Compact(ctx context.Context) (ports.ChatCompactionResult, error) {
	c.ids = append(c.ids, ports.ChatCompactionID(ctx))
	return ports.ChatCompactionResult{}, context.DeadlineExceeded
}

type observedSideClaimStore struct {
	SideStore
	checked chan string
}

func (s *observedSideClaimStore) ClaimNextSideTurn(ctx context.Context, runID string, excluded map[string]bool, at time.Time) (domain.SideTurn, domain.SideConversation, bool, error) {
	turn, side, found, err := s.SideStore.ClaimNextSideTurn(ctx, runID, excluded, at)
	turns, _, _ := s.SideTurns(ctx, "side", time.Time{}, 0)
	for _, candidate := range turns {
		if candidate.ClientMessageID == "queued-after-unknown" {
			select {
			case s.checked <- candidate.ID:
			default:
			}
		}
	}
	return turn, side, found, err
}
func TestSideUnknownCompactionRejectsRetryUntilOriginalSettles(t *testing.T) {
	ctx := t.Context()
	svc := New(Options{AppRunID: "compact-retry", Sessions: sideSessionReader{}, NewID: func() string { return "queued-turn" }})
	observed := &observedSideClaimStore{SideStore: svc.sides.store, checked: make(chan string, 1)}
	svc.sides.store = observed
	defer svc.sides.stopAll()
	at := time.Now().UTC()
	_, _ = svc.sides.store.ClaimSideLaunch(ctx, "compact-retry", at)
	side := domain.SideConversation{PolicyVersion: 1, ID: "side", SessionID: "session", MainConversationID: "main", AppRunID: "compact-retry", ProviderHostID: "side-host", ProviderForkID: "provider-side-1", State: "opening", Generation: "g"}
	if _, _, err := svc.sides.store.CreateSideConversation(ctx, side); err != nil {
		t.Fatal(err)
	}
	if err := svc.sides.store.SetSideReady(ctx, side.ID, side.Generation, side.ProviderForkID, at); err != nil {
		t.Fatal(err)
	}
	conv := &failedCompactRetryConversation{recoveredSideCloseConversation: recoveredSideCloseConversation{events: make(chan ports.ChatEvent)}}
	if !svc.sides.attach(side, conv) {
		t.Fatal("attach failed")
	}
	if _, err := svc.CompactSideChat(ctx, side.SessionID, side.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if _, err := svc.CompactSideChat(ctx, side.SessionID, side.ID); !errors.Is(err, ErrCompactionWhileBusy) {
		t.Fatalf("unknown compact overwritten: %v", err)
	}
	if len(conv.ids) != 1 {
		t.Fatalf("retry forwarded: %v", conv.ids)
	}
	// A regular queued question must not replace the unknown compact either.
	go svc.sides.schedule()
	queued, err := svc.SendSideQuestion(ctx, side.SessionID, side.ID, ports.ChatUserMessage{Text: "preserved input", ClientMessageID: "queued-after-unknown"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-observed.checked:
		if id != queued.ID {
			t.Fatalf("input identity changed: %s", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not inspect queued input")
	}
	turns, _, err := svc.sides.store.SideTurns(ctx, side.ID, time.Time{}, 0)
	if err != nil || len(turns) != 1 || turns[0].ID != queued.ID || turns[0].State != "queued" || turns[0].Text != "preserved input" {
		t.Fatalf("unknown compact lost intake: %+v %v", turns, err)
	}
	conv.mu.Lock()
	sent := conv.sent
	conv.mu.Unlock()
	if sent != 0 {
		t.Fatalf("new input reached provider before original compact settled: %d", sent)
	}
	if err := svc.CancelQueuedSideQuestion(ctx, side.SessionID, side.ID, queued.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.StopAll(ctx); !errors.Is(err, ports.ErrChatShutdownNotSettled) || conv.closed {
		t.Fatalf("unknown lost: %v closed=%v", err, conv.closed)
	}
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, CompactionID: "other", ProviderTurnID: "other", TurnState: domain.TurnStateCompleted}
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "marker", TurnState: domain.TurnStateCompleted}
	if err := svc.StopAll(ctx); !errors.Is(err, ports.ErrChatShutdownNotSettled) || conv.closed {
		t.Fatalf("unrelated completion erased unknown: %v", err)
	}
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, CompactionID: conv.ids[0], ProviderTurnID: "original", TurnState: domain.TurnStateCompleted}
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "marker", TurnState: domain.TurnStateCompleted}
	if err := svc.StopAll(ctx); err != nil || !conv.closed {
		t.Fatalf("original completion did not settle: %v closed=%v", err, conv.closed)
	}
}

type nativeReconciledSideConversation struct {
	recoveredSideCloseConversation
	idle bool
}

func (c *nativeReconciledSideConversation) ReconcileOutbound(context.Context) error {
	if !c.idle {
		return ports.ErrChatShutdownNotSettled
	}
	return nil
}
func TestSideCompactionRequiresNativeReconciliationBeforeStop(t *testing.T) {
	svc := New(Options{AppRunID: "native-recovery", Sessions: sideSessionReader{}})
	defer svc.sides.stopAll()
	at := time.Now().UTC()
	_, _ = svc.sides.store.ClaimSideLaunch(t.Context(), "native-recovery", at)
	side := domain.SideConversation{PolicyVersion: 1, ID: "side", SessionID: "session", MainConversationID: "main", AppRunID: "native-recovery", ProviderHostID: "side-host", ProviderForkID: "provider-side-1", State: "opening", Generation: "g"}
	if _, _, err := svc.sides.store.CreateSideConversation(t.Context(), side); err != nil {
		t.Fatal(err)
	}
	if err := svc.sides.store.SetSideReady(t.Context(), side.ID, side.Generation, side.ProviderForkID, at); err != nil {
		t.Fatal(err)
	}
	conv := &nativeReconciledSideConversation{recoveredSideCloseConversation: recoveredSideCloseConversation{events: make(chan ports.ChatEvent)}}
	if !svc.sides.attach(side, conv) {
		t.Fatal("attach failed")
	}
	if _, err := svc.CompactSideChat(t.Context(), side.SessionID, side.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.StopAll(t.Context()); !errors.Is(err, ports.ErrChatShutdownNotSettled) || conv.closed {
		t.Fatalf("unknown compact detached: %v", err)
	}
	conv.idle = true
	if err := svc.StopAll(t.Context()); err != nil || !conv.closed {
		t.Fatalf("native proof did not release compact: %v closed=%v", err, conv.closed)
	}
}
