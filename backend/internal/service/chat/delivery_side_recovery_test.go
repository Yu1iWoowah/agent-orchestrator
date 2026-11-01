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
	reconnected := &recoveredSideCloseConversation{events: make(chan ports.ChatEvent, 1)}
	if !after.sides.attach(recovered[0], reconnected) {
		t.Fatal("attach failed")
	}
	if err := after.StopAll(ctx); !errors.Is(err, ports.ErrChatShutdownNotSettled) {
		t.Fatalf("side compaction gap lost across JSON recovery: StopAll=%v transportClosed=%v", err, reconnected.closed)
	}
	// An empty custody flush is no execution receipt. Only the original native
	// completion can retire this gap, including after recovery lost compacting.
	reconnected.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "original-compaction", TurnState: domain.TurnStateCompleted}
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
