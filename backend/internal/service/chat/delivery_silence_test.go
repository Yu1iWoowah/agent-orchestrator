package chat_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestSilenceBudgetUsesNativeProgressTime(t *testing.T) {
	clock := newSettingsTestClock()
	conv := &stalledDeliveryConversation{fakeConversation: newFakeConversation(), queryCount: make(chan string, 8)}
	h := deliveryHarness(t, conv, clock)
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "original input", ClientMessageID: "delayed-observation"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.st.ChatDeliveryAttemptForTurn(t.Context(), testSession, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	clock.awaitTimer(t, 30*time.Second)
	clock.advance(time.Second)
	eventAt := clock.Now()
	conv.mu.Lock()
	receipt := conv.deliveryReceipts[a.ID]
	receipt.Source = "provider_event"
	receipt.Progress = "running"
	receipt.ProgressSequence = 1
	receipt.ObservedAt = eventAt
	conv.deliveryReceipts[a.ID] = receipt
	conv.mu.Unlock()
	clock.advance(29 * time.Second)
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		saved, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
		return e == nil && saved.Receipt != nil && saved.Receipt.ProgressSequence == 1
	})
	saved, err := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.LastProgressAt.Equal(eventAt) {
		t.Fatalf("query refreshed silence budget: nativeAt=%s queryAt=%s persistedLastProgress=%s next=%v", eventAt, clock.Now(), saved.LastProgressAt, saved.NextCheckAt)
	}
	if saved.NextCheckAt == nil || !saved.NextCheckAt.Equal(eventAt.Add(30*time.Second)) {
		t.Fatalf("deadline did not follow native event: %+v", saved)
	}
	clock.awaitTimer(t, time.Second)
	clock.advance(time.Second)
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		a, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, saved.ID)
		return e == nil && a.AttentionID != "" && a.LastProgressAt.Equal(eventAt)
	})
	if saved.Disposition != domain.ChatDeliveryAccepted {
		t.Fatal("accepted changed")
	}
}

func TestNativeApprovalWaitKeepsEventTimeAndRecoversSilence(t *testing.T) {
	clock := newSettingsTestClock()
	conv := newFakeConversation()
	h := deliveryHarness(t, conv, clock)
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "native approval"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.st.ChatDeliveryAttemptForTurn(t.Context(), testSession, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	clock.awaitTimer(t, 30*time.Second)
	clock.advance(time.Second)
	eventAt := clock.Now()
	conv.mu.Lock()
	r := conv.deliveryReceipts[a.ID]
	r.Progress = "approval"
	r.ProgressSequence = 1
	r.ObservedAt = eventAt
	conv.deliveryReceipts[a.ID] = r
	conv.mu.Unlock()
	clock.advance(29 * time.Second)
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		saved, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
		return e == nil && saved.Receipt.Progress == "approval"
	})
	waiting, err := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
	if err != nil || waiting.AttentionID != "" || !waiting.LastProgressAt.Equal(eventAt) {
		t.Fatalf("native wait invented activity or attention: %+v %v", waiting, err)
	}
	// Once the host no longer qualifies that wait, an unchanged historical event
	// cannot retain its later state-check deadline or hide the expired silence.
	conv.mu.Lock()
	r = conv.deliveryReceipts[a.ID]
	r.Progress = "running"
	conv.deliveryReceipts[a.ID] = r
	conv.mu.Unlock()
	clock.advance(time.Second)
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady}
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		saved, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
		return e == nil && saved.AttentionID != "" && saved.LastProgressAt.Equal(eventAt)
	})
}
