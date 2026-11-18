package chat_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// goneHostConversation is a provider whose original host was replaced (a
// reboot or a host restart): the attached host has a new epoch and has never
// heard of attempts the old host carried.
type goneHostConversation struct {
	*fakeConversation
	epoch string
}

func (c *goneHostConversation) OutboundProviderEpoch() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

func (c *goneHostConversation) replaceHost() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch = "replacement-host-epoch"
	c.deliveryReceipts = nil
}

// An attempt left by a host that no longer exists ends with the daemon's
// incomplete receipt (sdr-total-review-1 q1=b): through shutdown preflight
// and through the delivery monitor, without a question or a replay. The code
// tells a completed turn from one the host loss interrupted (SDR-PR1 AC3b).
func TestGoneHostAttemptSettlesWithoutQuestionOrReplay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		member domain.TurnState
		code   string
		path   string
	}{
		{"completed via preflight", domain.TurnStateCompleted, "CHAT_DELIVERY_HOST_GONE_SETTLED", "preflight"},
		{"interrupted via preflight", domain.TurnStateFailed, "CHAT_DELIVERY_HOST_GONE_INTERRUPTED", "preflight"},
		{"completed via monitor after its question", domain.TurnStateCompleted, "CHAT_DELIVERY_HOST_GONE_SETTLED", "monitor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newSettingsTestClock()
			conv := &goneHostConversation{fakeConversation: newFakeConversation(), epoch: "fake-native-epoch"}
			h := deliveryHarness(t, conv, clock)
			// ACP shape: the old host recorded the prompt (unknown, incomplete,
			// epoch and request id) but never saw it settle.
			conv.setDeliveryClock(clock.Now, true)
			turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "before the reboot", ClientMessageID: "gone-" + tc.path})
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := h.st.ChatDeliveryAttemptForTurn(t.Context(), testSession, turn.ID)
			if err != nil || attempt.ExecutionSettled || attempt.Receipt == nil || attempt.Receipt.ProviderEpoch != "fake-native-epoch" {
				t.Fatalf("old-host attempt = %+v %v", attempt, err)
			}
			questionID := ""
			if tc.path == "monitor" {
				// While the original host still answers, silence opens the usual question.
				clock.awaitTimer(t, 30*time.Second)
				h.ctrl.AdvanceDeliveryClockForTest(func() { clock.advance(30 * time.Second) })
				h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
					a, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, attempt.ID)
					questionID = a.AttentionID
					return e == nil && a.AttentionID != ""
				})
			}
			// The turn ended (or was cut short) before the old host went away.
			conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: tc.member}
			h.awaitSnapshot(t, func(snap store.ConversationSnapshot) bool {
				return len(snap.Turns) == 1 && snap.Turns[0].State == tc.member
			})
			conv.replaceHost()
			if tc.path == "monitor" {
				conv.events <- ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady}
				h.awaitSnapshot(t, func(_ store.ConversationSnapshot) bool {
					a, e := h.st.ChatDeliveryAttempt(t.Context(), testSession, attempt.ID)
					return e == nil && a.ExecutionSettled
				})
			} else if err := h.svc.StopAll(t.Context()); err != nil {
				t.Fatalf("shutdown preflight blocked by a gone host's attempt: %v", err)
			}
			saved, err := h.st.ChatDeliveryAttempt(t.Context(), testSession, attempt.ID)
			if err != nil || !saved.ExecutionSettled || saved.ErrorCode != tc.code || saved.Disposition != domain.ChatDeliveryUnknown ||
				saved.Receipt == nil || saved.Receipt.Source != "incomplete" || saved.Receipt.ProviderEpoch != "fake-native-epoch" {
				t.Fatalf("gone-host settlement = %+v receipt=%+v %v", saved, saved.Receipt, err)
			}
			if pending, err := h.st.PendingChatDeliveries(t.Context(), testSession); err != nil || len(pending) != 0 {
				t.Fatalf("settled attempt still pending: %+v %v", pending, err)
			}
			if questionID != "" {
				question, err := h.st.GetAttentionQuestion(t.Context(), testSession, questionID)
				if err != nil || question.State != "cancelled" || question.CloseReason != "原執行已確認結束" {
					t.Fatalf("delivery question = %+v %v", question, err)
				}
			} else if saved.AttentionID != "" {
				t.Fatalf("a gone host's attempt opened a question: %+v", saved)
			}
			if len(conv.sentTexts()) != 1 {
				t.Fatal("input replayed after the host was gone")
			}
		})
	}
}

// The attached host's own unsettled attempt is not proof of anything.
func TestSameHostUnsettledAttemptStillBlocksPreflight(t *testing.T) {
	clock := newSettingsTestClock()
	conv := &goneHostConversation{fakeConversation: newFakeConversation(), epoch: "fake-native-epoch"}
	h := deliveryHarness(t, conv, clock)
	conv.setDeliveryClock(clock.Now, true)
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "still the same host", ClientMessageID: "same-host"})
	if err != nil {
		t.Fatal(err)
	}
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateCompleted}
	h.awaitSnapshot(t, func(snap store.ConversationSnapshot) bool {
		return len(snap.Turns) == 1 && snap.Turns[0].State == domain.TurnStateCompleted
	})
	if err := h.svc.StopAll(t.Context()); err == nil {
		t.Fatal("unsettled attempt of the attached host passed preflight")
	}
	saved, err := h.st.ChatDeliveryAttemptForTurn(t.Context(), testSession, turn.ID)
	if err != nil || saved.ExecutionSettled {
		t.Fatalf("same-host attempt settled without evidence: %+v %v", saved, err)
	}
}

// The new app's takeover request is the same POST /shutdown: once the only
// unsettled record belongs to a gone host it is accepted; a turn still in
// flight keeps the 409 (SDR-PR1 AC4).
func TestShutdownRouteAcceptsTakeoverAfterGoneHost(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		t.Run(map[bool]string{false: "gone-host record", true: "turn in flight"}[inFlight], func(t *testing.T) {
			clock := newSettingsTestClock()
			conv := &goneHostConversation{fakeConversation: newFakeConversation(), epoch: "fake-native-epoch"}
			h := deliveryHarness(t, conv, clock)
			conv.setDeliveryClock(clock.Now, true)
			turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "before the reboot", ClientMessageID: "takeover"})
			if err != nil {
				t.Fatal(err)
			}
			if !inFlight {
				conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateCompleted}
				h.awaitSnapshot(t, func(snap store.ConversationSnapshot) bool {
					return len(snap.Turns) == 1 && snap.Turns[0].State == domain.TurnStateCompleted
				})
				conv.replaceHost()
			}
			var requested atomic.Bool
			router := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, httpd.APIDeps{},
				httpd.ControlDeps{RequestShutdown: func() { requested.Store(true) }, ShutdownPreflight: h.svc.PreflightShutdown})
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/shutdown", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			want := http.StatusAccepted
			if inFlight {
				want = http.StatusConflict
			}
			if response.Code != want {
				t.Fatalf("POST /shutdown = %d %s, want %d", response.Code, response.Body.String(), want)
			}
			// The accepted handler requests shutdown just after its response.
			for deadline := time.Now().Add(2 * time.Second); !inFlight && !requested.Load() && time.Now().Before(deadline); {
				time.Sleep(5 * time.Millisecond)
			}
			if requested.Load() == inFlight {
				t.Fatalf("shutdown requested=%v with a turn in flight=%v", requested.Load(), inFlight)
			}
		})
	}
}
