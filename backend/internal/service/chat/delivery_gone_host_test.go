package chat_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/persistenthost"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type previousBootFaultStore struct {
	*store.Store
	failedOnce atomic.Bool
}

func (s *previousBootFaultStore) SettlePreviousBootAcceptance(ctx context.Context, attempt domain.ChatDeliveryAttempt, owner domain.AgentSettingsOwner, at time.Time) error {
	if !s.failedOnce.Swap(true) {
		return fmt.Errorf("fixture-private-cas-cause: %w", ports.ErrAgentSettingsConflict)
	}
	return s.Store.SettlePreviousBootAcceptance(ctx, attempt, owner, at)
}

func TestHibernateChecksPreviousBootAcceptanceBeforeDeliveryHold(t *testing.T) {
	requireBootIdentity(t)
	h, conv := settledHibernationHarness(t, "")
	conv.mu.Lock()
	conv.outboundEpoch, conv.deliveryEpoch = previousBootEpoch, previousBootEpoch
	conv.mu.Unlock()
	ctx := t.Context()
	turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "old accepted work", ClientMessageID: "hibernate-old-input"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := h.st.ChatDeliveryAttemptForTurn(ctx, testSession, turn.ID)
	if err != nil || before.ExecutionSettled || before.Receipt == nil || before.Receipt.ProviderEpoch != previousBootEpoch {
		t.Fatalf("missing original acceptance: %+v, %v", before, err)
	}
	// Native history can close the turn without the host's original journal
	// recording a completion receipt. Preserve that accepted receipt verbatim.
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateCompleted}
	h.awaitSnapshot(t, func(snap store.ConversationSnapshot) bool {
		return len(snap.Turns) == 1 && snap.Turns[0].State == domain.TurnStateCompleted
	})
	if err := h.svc.DrainQueued(ctx, testSession); err != nil {
		t.Fatal(err)
	}
	conv.replaceProviderHost()
	if slept, err := h.svc.HibernateChatForUpdate(ctx, testSession); err != nil || !slept {
		t.Fatalf("previous boot hold blocked update: hibernated=%v, %v", slept, err)
	}
	after, err := h.st.ChatDeliveryAttempt(ctx, testSession, before.ID)
	if err != nil || !after.ExecutionSettled || after.ErrorCode != "CHAT_DELIVERY_HOST_GONE_SETTLED" || after.Receipt == nil || *after.Receipt != *before.Receipt || len(conv.sentTexts()) != 1 {
		t.Fatalf("hibernation changed or re-dispatched the original acceptance: %+v, %v", after, err)
	}
}

// previousBootEpoch is a real host epoch recorded in another boot (its boot id
// is not this machine's), judged by the real persistent-host rule.
const previousBootEpoch = "host-4242-1700000000000000000-00000000-0000-0000-0000-000000000000"

// sameBootEpoch is a real host epoch from this boot: a host that crashed or was
// killed now, whose provider may still be running in its own process group.
func sameBootEpoch(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Skip("boot identity is recorded on Linux only")
	}
	return fmt.Sprintf("host-4242-%d-%s", time.Now().UnixNano(), strings.TrimSpace(string(raw)))
}

func requireBootIdentity(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("boot identity is recorded on Linux only")
	}
}

// goneHostConversation starts attached to the host that recorded its
// attempts; replaceProviderHost then models the next host.
type goneHostConversation struct {
	*fakeConversation
}

func newGoneHostConversation(epoch string) *goneHostConversation {
	conv := &goneHostConversation{fakeConversation: newFakeConversation()}
	conv.outboundEpoch, conv.deliveryEpoch = epoch, epoch
	return conv
}

// An attempt left by a host that no longer exists ends with the daemon's
// incomplete receipt (sdr-total-review-1 q1=b): through shutdown preflight
// and through the delivery monitor, without a question or a replay. The code
// tells a completed turn from one the host loss interrupted (SDR-PR1 AC3b).
func TestGoneHostAttemptSettlesWithoutQuestionOrReplay(t *testing.T) {
	requireBootIdentity(t)
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
			conv := newGoneHostConversation(previousBootEpoch)
			h := deliveryHarness(t, conv, clock)
			// ACP shape: the old host recorded the prompt (unknown, incomplete,
			// epoch and request id) but never saw it settle.
			conv.setDeliveryClock(clock.Now, true)
			turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "before the reboot", ClientMessageID: "gone-" + tc.path})
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := h.st.ChatDeliveryAttemptForTurn(t.Context(), testSession, turn.ID)
			if err != nil || attempt.ExecutionSettled || attempt.Receipt == nil || attempt.Receipt.ProviderEpoch != previousBootEpoch {
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
			conv.replaceProviderHost()
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
				saved.Receipt == nil || saved.Receipt.Source != "incomplete" || saved.Receipt.ProviderEpoch != previousBootEpoch {
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
	requireBootIdentity(t)
	clock := newSettingsTestClock()
	conv := newGoneHostConversation(previousBootEpoch)
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

func TestPreviousBootAcceptedKeepsOriginalReceipt(t *testing.T) {
	requireBootIdentity(t)
	clock := newSettingsTestClock()
	conv := newGoneHostConversation(previousBootEpoch)
	h := deliveryHarness(t, conv, clock)
	ctx := t.Context()
	turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "already accepted", ClientMessageID: "accepted-before-boot"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := h.st.ChatDeliveryAttemptForTurn(ctx, testSession, turn.ID)
	if err != nil || before.Receipt == nil || before.Disposition != domain.ChatDeliveryAccepted || before.ExecutionSettled {
		t.Fatalf("original acceptance: %+v, %v", before, err)
	}
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateCompleted}
	h.awaitSnapshot(t, func(snap store.ConversationSnapshot) bool {
		return len(snap.Turns) == 1 && snap.Turns[0].State == domain.TurnStateCompleted
	})
	conv.replaceProviderHost()
	if err := h.svc.PreflightShutdown(ctx); err != nil {
		t.Fatalf("old accepted entry blocked shutdown: %v", err)
	}
	after, err := h.st.ChatDeliveryAttempt(ctx, testSession, before.ID)
	if err != nil || !after.ExecutionSettled || after.Receipt == nil || *after.Receipt != *before.Receipt ||
		after.Disposition != before.Disposition || after.EvidenceLevel != before.EvidenceLevel || after.AttentionID != "" {
		t.Fatalf("local settlement changed the original acceptance: before=%+v after=%+v, %v", before, after, err)
	}
	if sent := conv.sentTexts(); len(sent) != 1 {
		t.Fatalf("provider dispatches=%v; want only the original dispatch", sent)
	}
}

func TestColdShutdownReconcilesAcceptanceWithoutResuming(t *testing.T) {
	requireBootIdentity(t)
	for _, previous := range []bool{true, false} {
		t.Run(fmt.Sprint(previous), func(t *testing.T) {
			epoch := previousBootEpoch
			if !previous {
				epoch = sameBootEpoch(t)
			}
			clock := newSettingsTestClock()
			conv := newGoneHostConversation(epoch)
			h := deliveryHarness(t, conv, clock)
			ctx := t.Context()
			turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "one original input", ClientMessageID: "cold-input"})
			if err != nil {
				t.Fatal(err)
			}
			original, err := h.st.ChatDeliveryAttemptForTurn(ctx, testSession, turn.ID)
			if err != nil || original.Receipt == nil {
				t.Fatalf("acceptance=%+v, %v", original, err)
			}
			fault := &previousBootFaultStore{Store: h.st}
			cold := chatsvc.New(chatsvc.Options{Store: fault, Sessions: h.st, PreviousBootEpoch: persistenthost.EpochFromPreviousBoot, Now: clock.Now})
			if previous {
				err := cold.ReconcilePreviousBootDeliveries(ctx)
				var blocked *ports.ChatShutdownBlockedError
				if !errors.As(err, &blocked) || blocked.SessionID != string(testSession) || blocked.Reason != "delivery_journal_unavailable" || strings.Contains(err.Error(), "fixture-private-cas-cause") {
					t.Fatalf("temporary CAS failure did not block safely: %v", err)
				}
				held, err := h.st.ChatDeliveryAttempt(ctx, testSession, original.ID)
				if err != nil || held.ExecutionSettled || held.AttentionID != "" || held.Version != original.Version || held.Receipt == nil || *held.Receipt != *original.Receipt {
					t.Fatalf("failed settlement modified original attempt: %+v, %v", held, err)
				}
			}
			if err := cold.ReconcilePreviousBootDeliveries(ctx); err != nil {
				t.Fatal(err)
			}
			if err := cold.PreflightShutdown(ctx); err != nil {
				t.Fatal(err)
			}
			saved, err := h.st.ChatDeliveryAttempt(ctx, testSession, original.ID)
			if err != nil || saved.ExecutionSettled != previous || saved.Receipt == nil || *saved.Receipt != *original.Receipt {
				t.Fatalf("cold acceptance=%+v, %v; previous=%t", saved, err, previous)
			}
			row, err := h.st.TurnByID(ctx, turn.ID)
			if err != nil || previous && row.State != domain.TurnStateInterrupted || !previous && row.State != domain.TurnStateRunning {
				t.Fatalf("turn after cold boot=%s, %v; previous=%t", row.State, err, previous)
			}
			if len(conv.sentTexts()) != 1 || cold.HasLiveChatController(testSession) {
				t.Fatal("cold reconciliation resumed or re-dispatched the original input")
			}
		})
	}
}

type cancelledBindingStore struct{ *store.Store }

func (*cancelledBindingStore) BindTurnToProvider(context.Context, string, string, time.Time) error {
	return context.Canceled
}

func TestColdShutdownRecoversAcceptedReceiptBeforeProviderBinding(t *testing.T) {
	for _, tc := range []struct {
		name           string
		reconnect      bool
		projectStarted bool
	}{
		{name: "running"},
		{name: "after live reconnect", reconnect: true},
		{name: "after TurnStarted", projectStarted: true},
		{name: "after reconnect replay", reconnect: true, projectStarted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testColdShutdownRecoversUnboundAcceptance(t, tc.reconnect, tc.projectStarted)
		})
	}
}

func testColdShutdownRecoversUnboundAcceptance(t *testing.T, reconnect, projectStarted bool) {
	t.Helper()
	requireBootIdentity(t)
	ctx := t.Context()
	clock := newSettingsTestClock()
	conv := newGoneHostConversation(previousBootEpoch)
	h := newHarnessWithConversationAndStoreForHarness(t, conv, func(st *store.Store) chatsvc.Store {
		return &cancelledBindingStore{Store: st}
	}, domain.HarnessCodex, func(opts *chatsvc.Options) { opts.Now, opts.After = clock.Now, clock.After })

	turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "accepted before binding", ClientMessageID: "cold-unbound"})
	if !errors.Is(err, ports.ErrChatDeliveryUnverified) || turn.ID == "" {
		t.Fatalf("Send = %q, %v; want retained input after failed binding", turn.ID, err)
	}
	before, err := h.st.TurnByID(ctx, turn.ID)
	if err != nil || before.State != domain.TurnStateRunning || before.ProviderTurnID != "" {
		t.Fatalf("pre-reboot turn = %s/%q, %v; want running without a provider binding", before.State, before.ProviderTurnID, err)
	}
	original, err := h.st.ChatDeliveryAttemptForTurn(ctx, testSession, turn.ID)
	if err != nil || original.Disposition != domain.ChatDeliveryAccepted || original.ExecutionSettled ||
		original.Receipt == nil || original.Receipt.ProviderTurnID == "" || original.Receipt.ProviderEpoch != previousBootEpoch {
		t.Fatalf("accepted receipt missing before reboot: %+v, %v", original, err)
	}

	if !reconnect && !projectStarted {
		stale := original
		stale.Version--
		if err := h.st.SettlePreviousBootAcceptance(ctx, stale, original.Owner, clock.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
			t.Fatalf("settlement with a stale version: %v", err)
		}
		staleOwner := original.Owner
		staleOwner.ControllerGeneration = "stale-controller"
		if err := h.st.SettlePreviousBootAcceptance(ctx, original, staleOwner, clock.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
			t.Fatalf("settlement with a stale owner: %v", err)
		}
		held, err := h.st.TurnByID(ctx, turn.ID)
		if err != nil || held.ProviderTurnID != "" || held.State != domain.TurnStateRunning {
			t.Fatalf("a rejected settlement changed the unbound turn: %s/%q, %v", held.State, held.ProviderTurnID, err)
		}
		const conflictingProviderID = "other-provider-turn"
		if err := h.st.BindTurnToProvider(ctx, turn.ID, conflictingProviderID, clock.Now()); err != nil {
			t.Fatal(err)
		}
		if err := h.st.SettlePreviousBootAcceptance(ctx, original, original.Owner, clock.Now()); !errors.Is(err, ports.ErrAgentSettingsConflict) {
			t.Fatalf("settlement overwrote an existing provider binding: %v", err)
		}
		conflicting, err := h.st.TurnByID(ctx, turn.ID)
		if err != nil || conflicting.ProviderTurnID != conflictingProviderID || conflicting.State != domain.TurnStateRunning {
			t.Fatalf("a conflicting settlement changed the turn: %s/%q, %v", conflicting.State, conflicting.ProviderTurnID, err)
		}
		if err := h.st.BindTurnToProvider(ctx, turn.ID, "", clock.Now()); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := h.st.PendingChatDeliveries(ctx, testSession)
	if err != nil || len(pending) != 1 || pending[0].ID != original.ID {
		t.Fatalf("the rejected settlements changed the original hold: %+v, %v", pending, err)
	}

	wantState := domain.TurnStateInterrupted
	var reconnectProvider *reconnectedReceiptConversation
	var reconnectTurn domain.ConversationTurn
	var adoptedID string
	const approvalItem = "adopted-approval"
	projectProviderTurn := func(provider *fakeConversation) {
		providerTurn := original.Receipt.ProviderTurnID
		provider.emit(
			ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: providerTurn, ProviderEventID: "adopted-start"},
			ports.ChatEvent{Kind: ports.ChatEventApprovalRequested, ProviderTurnID: providerTurn, ProviderEventID: "adopted-approval-request",
				ProviderItemID: approvalItem, RequestID: approvalItem, ActivityStatus: domain.ActivityStatusPending},
		)
		h.awaitSnapshot(t, func(snapshot store.ConversationSnapshot) bool {
			for _, projected := range snapshot.Turns {
				if projected.ID != turn.ID && projected.ProviderTurnID == providerTurn && projected.State == domain.TurnStateRunning {
					adoptedID = projected.ID
					break
				}
			}
			return adoptedID != "" && len(snapshot.Activities) == 1 &&
				snapshot.Activities[0].TurnID == adoptedID && snapshot.Activities[0].Status == domain.ActivityStatusPending
		})
	}
	if projectStarted && !reconnect {
		projectProviderTurn(conv.fakeConversation)
		conv.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerStopped, Err: ports.ErrChatRecoveryInconclusive})
		if err := conv.Close(); err != nil {
			t.Fatal(err)
		}
		h.ctrl.Wait()
	}
	if reconnect {
		// Close the old daemon's attachment without letting ordinary projector
		// cleanup settle its work. The first reconnect is still on the same boot.
		conv.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerStopped, Err: ports.ErrChatRecoveryInconclusive})
		if err := conv.Close(); err != nil {
			t.Fatal(err)
		}
		h.ctrl.Wait()
		reconnectProvider = &reconnectedReceiptConversation{receiptConversation: &receiptConversation{
			terminatingConversation: &terminatingConversation{fakeConversation: newFakeConversation()},
			ack:                     func(string) error { return nil },
		}}
		reconnectProvider.outboundEpoch = previousBootEpoch
		var ids atomic.Int64
		reconnected := chatsvc.New(chatsvc.Options{
			Store: h.st, Sessions: h.st, Reader: fullSnapshotReader(h.st),
			Drivers: fakeRegistry{driver: fakeDriver{conv: reconnectProvider}},
			Now:     clock.Now, After: clock.After, NewID: func() string { return fmt.Sprintf("same-boot-reconnect-%d", ids.Add(1)) },
		})
		controller, err := reconnected.Start(ctx, chatsvc.StartConfig{
			SessionID: testSession, ProjectID: testProject, Harness: domain.HarnessCodex,
			WorkspacePath: t.TempDir(), ProviderConversationID: conv.ProviderConversationID(),
		})
		if err != nil {
			t.Fatalf("same-boot live reconnect: %v", err)
		}
		t.Cleanup(func() { _ = reconnectProvider.Close(); controller.Wait() })
		reconnectTurn, err = h.st.TurnByID(ctx, turn.ID)
		if err != nil || reconnectTurn.State != domain.TurnStateFailed || reconnectTurn.ProviderTurnID != "" || reconnectTurn.StartedAt != nil || reconnectTurn.CompletedAt == nil {
			t.Fatalf("reconnect cleanup turn = %+v, %v; want failed without a binding", reconnectTurn, err)
		}
		stillHeld, err := h.st.ChatDeliveryAttempt(ctx, testSession, original.ID)
		if err != nil || stillHeld.ExecutionSettled || stillHeld.Receipt == nil || *stillHeld.Receipt != *original.Receipt {
			t.Fatalf("same-boot reconnect changed the receipt or hold: %+v, %v", stillHeld, err)
		}
		if projectStarted {
			projectProviderTurn(reconnectProvider.fakeConversation)
		}
		reconnectProvider.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerStopped, Err: ports.ErrChatRecoveryInconclusive})
		if err := reconnectProvider.Close(); err != nil {
			t.Fatal(err)
		}
		controller.Wait()
		wantState = domain.TurnStateFailed
	}

	cold := chatsvc.New(chatsvc.Options{Store: h.st, Sessions: h.st, PreviousBootEpoch: persistenthost.EpochFromPreviousBoot, Now: clock.Now})
	requested := make(chan struct{})
	router := httpd.NewRouterWithControl(config.Config{}, slog.New(slog.DiscardHandler), nil, httpd.APIDeps{}, httpd.ControlDeps{
		RequestShutdown: func() { close(requested) }, ShutdownPreflight: cold.PreflightShutdown,
	})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/shutdown", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("cold POST /shutdown = %d %s; want 202", response.Code, response.Body.String())
	}
	select {
	case <-requested:
	case <-time.After(2 * time.Second):
		t.Fatal("cold POST /shutdown did not request shutdown")
	}
	saved, err := h.st.ChatDeliveryAttempt(ctx, testSession, original.ID)
	if err != nil || !saved.ExecutionSettled || saved.ErrorCode != "CHAT_DELIVERY_HOST_GONE_INTERRUPTED" ||
		saved.Receipt == nil || *saved.Receipt != *original.Receipt {
		t.Fatalf("cold settlement changed the original receipt: %+v, %v", saved, err)
	}
	after, err := h.st.TurnByID(ctx, turn.ID)
	wantProviderID := original.Receipt.ProviderTurnID
	if projectStarted {
		wantProviderID = ""
	}
	if err != nil || after.State != wantState || after.ProviderTurnID != wantProviderID {
		t.Fatalf("recovered turn = %s/%q, %v", after.State, after.ProviderTurnID, err)
	}
	if reconnect && (after.ErrorMessage != reconnectTurn.ErrorMessage || after.StartedAt != nil || after.CompletedAt == nil || !reconnectTurn.CompletedAt.Equal(*after.CompletedAt)) {
		t.Fatalf("cold settlement rewrote the reconnect terminal result: %+v", after)
	}
	if projectStarted {
		adopted, err := h.st.TurnByID(ctx, adoptedID)
		if err != nil || adopted.State != domain.TurnStateInterrupted || adopted.ProviderTurnID != original.Receipt.ProviderTurnID {
			t.Fatalf("adopted turn after cold settlement = %s/%q, %v", adopted.State, adopted.ProviderTurnID, err)
		}
		snapshot, err := h.st.LoadConversationSnapshot(ctx, h.ctrl.ConversationID())
		if err != nil {
			t.Fatal(err)
		}
		approval := findActivity(t, snapshot, approvalItem)
		if approval.TurnID != adoptedID || approval.Status != domain.ActivityStatusFailed {
			t.Fatalf("adopted approval after cold settlement = %+v", approval)
		}
	}
	if pending, err := h.st.PendingChatDeliveries(ctx, testSession); err != nil || len(pending) != 0 {
		t.Fatalf("cold settlement left pending delivery: %+v, %v", pending, err)
	}
	if calls := conv.sendCallCount(); calls != 1 || cold.HasLiveChatController(testSession) {
		t.Fatalf("cold settlement resumed the provider or sent again: %d sends", calls)
	}
	if reconnect && reconnectProvider.sendCallCount() != 0 {
		t.Fatal("same-boot reconnect dispatched the original input again")
	}
}

// The new app's takeover request is the same POST /shutdown: once the only
// unsettled record belongs to a gone host it is accepted; a turn still in
// flight keeps the 409 (SDR-PR1 AC4).
func TestShutdownRouteAcceptsTakeoverAfterGoneHost(t *testing.T) {
	requireBootIdentity(t)
	for _, inFlight := range []bool{false, true} {
		t.Run(map[bool]string{false: "gone-host record", true: "turn in flight"}[inFlight], func(t *testing.T) {
			clock := newSettingsTestClock()
			conv := newGoneHostConversation(previousBootEpoch)
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
				conv.replaceProviderHost()
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
			if inFlight && (!strings.Contains(response.Body.String(), `"blockedSession":"`+string(testSession)+`"`) || !strings.Contains(response.Body.String(), `"blockedReason":"delivery_not_settled"`)) {
				t.Fatalf("409 does not name the blocking turn: %s", response.Body.String())
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

// Idle hibernation and the move to an installed harness release share the
// settlement: a gone host's attempt no longer keeps either from stopping the
// provider (SDR-PR1 AC1b, AC3b).
func TestGoneHostAttemptNoLongerBlocksHibernationOrVersionRefresh(t *testing.T) {
	requireBootIdentity(t)
	for _, path := range []string{"hibernation", "version refresh"} {
		t.Run(path, func(t *testing.T) {
			ctx := t.Context()
			var h *harness
			var conv *hibernationConversation
			if path == "hibernation" {
				h, conv = settledHibernationHarness(t, "")
			} else {
				h, conv, _ = versionRefreshHarness(t)
			}
			conv.outboundEpoch, conv.deliveryEpoch = previousBootEpoch, previousBootEpoch
			conv.setDeliveryClock(h.now, true)
			turn, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "before the reboot", ClientMessageID: "gone-" + path})
			if err != nil {
				t.Fatal(err)
			}
			// The turn ended, but the old host's settled receipt never arrived.
			conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateCompleted}
			h.awaitSnapshot(t, func(snap store.ConversationSnapshot) bool {
				for _, candidate := range snap.Turns {
					if candidate.ID == turn.ID {
						return candidate.State == domain.TurnStateCompleted
					}
				}
				return false
			})
			conv.replaceProviderHost()
			var stopped bool
			var stopErr error
			if path == "hibernation" {
				rec, found, err := h.st.GetSession(ctx, testSession)
				if err != nil || !found {
					t.Fatalf("get session = %v, %v", found, err)
				}
				rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: h.now().Add(-chatsvc.IdleBeforeHibernate)}
				if err := h.st.UpdateSession(ctx, rec); err != nil {
					t.Fatal(err)
				}
				stopped, stopErr = h.svc.HibernateChat(ctx, testSession)
			} else {
				markStale(t, h)
				h.advance(refreshIdle)
				stopped, stopErr = h.svc.HibernateChatForVersionRefresh(ctx, testSession, refreshIdle)
			}
			if stopErr != nil || !stopped {
				t.Fatalf("%s blocked by a gone host's attempt: stopped=%v err=%v", path, stopped, stopErr)
			}
			saved, err := h.st.ChatDeliveryAttemptForTurn(ctx, testSession, turn.ID)
			if err != nil || !saved.ExecutionSettled || saved.ErrorCode != "CHAT_DELIVERY_HOST_GONE_SETTLED" || saved.AttentionID != "" {
				t.Fatalf("gone-host attempt after %s = %+v %v", path, saved, err)
			}
		})
	}
}

// A host that died in this boot (SIGKILL, OOM) is replaced, but its provider
// runs in its own process group and may still be changing the workspace. Its
// attempt stays unconfirmed: preflight keeps refusing and nothing is settled
// or released (SDR-PR1 review P1).
func TestSameBootHostCrashKeepsAttemptUnconfirmed(t *testing.T) {
	requireBootIdentity(t)
	clock := newSettingsTestClock()
	epoch := sameBootEpoch(t)
	conv := newGoneHostConversation(epoch)
	h := deliveryHarness(t, conv, clock)
	conv.setDeliveryClock(clock.Now, true)
	turn, err := h.svc.Send(t.Context(), testSession, ports.ChatUserMessage{Text: "before the host crash", ClientMessageID: "same-boot"})
	if err != nil {
		t.Fatal(err)
	}
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateCompleted}
	h.awaitSnapshot(t, func(snap store.ConversationSnapshot) bool {
		return len(snap.Turns) == 1 && snap.Turns[0].State == domain.TurnStateCompleted
	})
	conv.replaceProviderHost()
	var blocked *ports.ChatShutdownBlockedError
	if err := h.svc.StopAll(t.Context()); !errors.As(err, &blocked) {
		t.Fatalf("a same-boot host crash passed preflight: %v", err)
	}
	saved, err := h.st.ChatDeliveryAttemptForTurn(t.Context(), testSession, turn.ID)
	if err != nil || saved.ExecutionSettled || strings.HasPrefix(saved.ErrorCode, "CHAT_DELIVERY_HOST_GONE") || saved.Receipt == nil || saved.Receipt.ProviderEpoch != epoch {
		t.Fatalf("same-boot attempt settled without proof the provider stopped: %+v %v", saved, err)
	}
	if len(conv.sentTexts()) != 1 {
		t.Fatal("input replayed")
	}
}
