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
	h.ctrl.AdvanceDeliveryClockForTest(func() { clock.advance(time.Second) })
	eventAt := clock.Now()
	conv.mu.Lock()
	receipt := conv.deliveryReceipts[a.ID]
	receipt.Source = "provider_event"
	receipt.Progress = "running"
	receipt.ProgressSequence = 1
	receipt.ObservedAt = eventAt
	conv.deliveryReceipts[a.ID] = receipt
	conv.mu.Unlock()
	h.ctrl.AdvanceDeliveryClockForTest(func() { clock.advance(29 * time.Second) })
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
	// The reachable host still owns this started execution: silence past the
	// budget rechecks in 30 seconds instead of opening a question.
	for len(conv.queryCount) > 0 {
		<-conv.queryCount
	}
	clock.awaitTimer(t, time.Second)
	h.ctrl.AdvanceDeliveryClockForTest(func() { clock.advance(time.Second) })
	for range 2 {
		if key := <-conv.queryCount; key != a.ID {
			t.Fatal(key)
		}
		clock.awaitTimer(t, 30*time.Second)
		silent, err := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
		if err != nil || silent.AttentionID != "" || silent.BlockedSince != nil || !silent.LastProgressAt.Equal(eventAt) {
			t.Fatalf("silent started execution opened a question or refreshed progress: %+v %v", silent, err)
		}
		h.ctrl.AdvanceDeliveryClockForTest(func() { clock.advance(30 * time.Second) })
	}
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
	h.ctrl.AdvanceDeliveryClockForTest(func() { clock.advance(time.Second) })
	eventAt := clock.Now()
	conv.mu.Lock()
	r := conv.deliveryReceipts[a.ID]
	r.Progress = "approval"
	r.ProgressSequence = 1
	r.ObservedAt = eventAt
	conv.deliveryReceipts[a.ID] = r
	conv.mu.Unlock()
	h.ctrl.AdvanceDeliveryClockForTest(func() { clock.advance(29 * time.Second) })
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		saved, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
		return e == nil && saved.Receipt.Progress == "approval"
	})
	waiting, err := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
	if err != nil || waiting.AttentionID != "" || !waiting.LastProgressAt.Equal(eventAt) {
		t.Fatalf("native wait invented activity or attention: %+v %v", waiting, err)
	}
	// Once the host no longer qualifies that wait, the unchanged historical event
	// keeps its time; the reachable started execution rechecks in 30 seconds
	// instead of opening a question for silence alone.
	conv.mu.Lock()
	r = conv.deliveryReceipts[a.ID]
	r.Progress = "running"
	conv.deliveryReceipts[a.ID] = r
	conv.mu.Unlock()
	h.ctrl.AdvanceDeliveryClockForTest(func() { clock.advance(time.Second) })
	recheck := clock.Now().Add(30 * time.Second)
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady}
	h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
		saved, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
		return e == nil && saved.Receipt.Progress == "running" && saved.NextCheckAt != nil && saved.NextCheckAt.Equal(recheck)
	})
	saved, err := h.st.ChatDeliveryAttempt(t.Context(), testSession, a.ID)
	if err != nil || saved.AttentionID != "" || saved.BlockedSince != nil || !saved.LastProgressAt.Equal(eventAt) {
		t.Fatalf("silent started execution opened a question or refreshed progress: %+v %v", saved, err)
	}
}
