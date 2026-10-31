package chat_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestAuditBindReceiptAndPiggyback(t *testing.T) {
	for _, mode := range []string{"direct", "queuedAhead", "queuedList", "queuedClaim"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			conv := &bindDeferredConversation{fakeConversation: newFakeConversation()}
			fault := &bindUnsentFaultStore{}
			readFault := &readyRetryFaultStore{atClaim: mode == "queuedClaim", persistent: true, requeued: make(chan struct{})}
			h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store {
				fault.Store = db
				if mode == "queuedList" || mode == "queuedClaim" {
					readFault.Store = fault
					return readFault
				}
				return fault
			})
			fence := func(id string) {
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "previous", ProviderItemID: id, ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: id})
				h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
					for _, a := range s.Activities {
						if a.Summary == id {
							return true
						}
					}
					return false
				})
			}
			if mode != "direct" {
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "previous", ProviderInitiated: true})
				h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) == 1 })
				if _, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "older queued input", ClientMessageID: "older"}); err != nil {
					t.Fatal(err)
				}
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "previous", TurnState: domain.TurnStateFailed})
				fence("seed-fence")
			}
			worker, err := h.st.CreateSession(ctx, domain.SessionRecord{ProjectID: testProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: h.now(), UpdatedAt: h.now()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.st.CreateReport(ctx, domain.ReportRecord{ID: "audit-report", SessionID: worker.ID, ProjectID: testProject, State: domain.ReportCheckpoint, Note: "audit-report-note", CreatedAt: h.now(), AvailableAt: h.now().Add(time.Hour), DeliveryState: domain.ReportPending, RepeatCount: 1}); err != nil {
				t.Fatal(err)
			}
			h.svc.SetReportCoordinator(reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{Store: h.st, Now: h.now, NewToken: func() string { return "audit-claim" }}))
			if mode == "queuedList" || mode == "queuedClaim" {
				readFault.armed.Store(true)
			} else {
				fault.armed.Store(true)
			}
			turn, firstErr := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "accepted input", ClientMessageID: "accepted", Origin: domain.MessageOriginHuman})
			saved, err := h.st.TurnByID(ctx, turn.ID)
			if err != nil {
				t.Fatal(err)
			}
			report, found, err := h.st.GetReport(ctx, "audit-report")
			if err != nil || !found {
				t.Fatalf("report: found=%v err=%v", found, err)
			}
			t.Logf("first receipt=%s durable=%s error=%v report=%s", turn.State, saved.State, firstErr, report.DeliveryState)
			if mode == "queuedList" || mode == "queuedClaim" {
				if readFault.attempts.Load() != 2 {
					t.Errorf("queue store retry was not bounded to two attempts: %d", readFault.attempts.Load())
				}
				readFault.armed.Store(false)
			}
			if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "trigger queue", ClientMessageID: "trigger", Origin: domain.MessageOriginHuman}); err != nil {
				t.Fatal(err)
			}
			fence("delivery-fence")
			copies := 0
			for _, m := range conv.sentMessages() {
				if strings.HasPrefix(m.Text, "AO pending messages in enqueue order.") {
					for _, item := range parseDeliveredBatch(t, m.Text) {
						copies += strings.Count(item.Text, "<ao-worker-reports>")
					}
				} else {
					copies += strings.Count(m.Text, "<ao-worker-reports>")
				}
			}
			if firstErr != nil || report.DeliveryState != domain.ReportAcknowledged || copies != 1 {
				t.Errorf("durably accepted message rejected: err=%v report-after-first=%s delivered-report-copies=%d", firstErr, report.DeliveryState, copies)
			}
		})
	}
}

func TestAuditIdleDirectBindFailureSameIDRetry(t *testing.T) {
	ctx := context.Background()
	conv := &bindDeferredConversation{fakeConversation: newFakeConversation()}
	fault := &bindUnsentFaultStore{}
	h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store { fault.Store = db; return fault })
	fault.armed.Store(true)
	msg := ports.ChatUserMessage{Text: "idle input", ClientMessageID: "idle-input"}
	turn, firstErr := h.ctrl.Send(ctx, msg)
	for range 3 {
		if _, err := h.ctrl.Send(ctx, msg); err != nil {
			t.Fatal(err)
		}
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "previous", ProviderItemID: "idle-fence", ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: "idle-fence"})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
		for _, a := range s.Activities {
			if a.Summary == "idle-fence" {
				return true
			}
		}
		return false
	})
	saved, err := h.st.TurnByID(ctx, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State == domain.TurnStateQueued || conv.starts.Load() != 1 {
		t.Errorf("no progress without synthetic Ready: state=%s starts=%d first-error=%v", saved.State, conv.starts.Load(), firstErr)
	}
}

type auditReadyBindConversation struct {
	*bindDeferredConversation
	refuse atomic.Bool
}

func (c *auditReadyBindConversation) SendTurn(ctx context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	if c.refuse.CompareAndSwap(true, false) {
		return ports.ChatTurnRef{}, ports.ErrChatTurnNotSentBusy
	}
	return c.bindDeferredConversation.SendTurn(ctx, msg)
}
func TestAuditBindFailureDuringLastReady(t *testing.T) {
	ctx := context.Background()
	conv := &auditReadyBindConversation{bindDeferredConversation: &bindDeferredConversation{fakeConversation: newFakeConversation()}}
	fault := &bindUnsentFaultStore{}
	h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store { fault.Store = db; return fault })
	fence := func(id string) {
		conv.emit(ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "previous", ProviderItemID: id, ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: id})
		h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
			for _, a := range s.Activities {
				if a.Summary == id {
					return true
				}
			}
			return false
		})
	}
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "previous", ProviderInitiated: true})
	h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) == 1 })
	turn, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "waiting input", ClientMessageID: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	conv.refuse.Store(true)
	fault.armed.Store(true)
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "previous", TurnState: domain.TurnStateCompleted})
	fence("completion-before-ready")
	conv.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady})
	fence("last-ready")
	saved, err := h.st.TurnByID(ctx, turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State == domain.TurnStateQueued || conv.starts.Load() != 1 {
		t.Errorf("last Ready consumed by Bind failure: state=%s starts=%d discards=%d", saved.State, conv.starts.Load(), conv.discards.Load())
	}
}

func TestLastReadyRetryIsBounded(t *testing.T) {
	for _, mode := range []string{"bind", "list", "claim"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			conv := &auditReadyBindConversation{bindDeferredConversation: &bindDeferredConversation{fakeConversation: newFakeConversation()}}
			fault := &bindUnsentFaultStore{}
			readFault := &readyRetryFaultStore{atClaim: mode == "claim", persistent: true, requeued: make(chan struct{})}
			h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store {
				fault.Store = db
				if mode != "bind" {
					readFault.Store = fault
					return readFault
				}
				return fault
			})
			fence := func(id string) {
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "previous", ProviderItemID: id, ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: id})
				h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
					for _, a := range s.Activities {
						if a.Summary == id {
							return true
						}
					}
					return false
				})
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "previous", ProviderInitiated: true})
			fence("previous-started")
			turn, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "retain waiting input", ClientMessageID: "waiting"})
			if err != nil {
				t.Fatal(err)
			}
			conv.refuse.Store(true)
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "previous", TurnState: domain.TurnStateCompleted})
			fence("completion-refused")
			if mode == "bind" {
				fault.failAlways.Store(true)
				fault.onFault = func() {
					// Let an over-retrying implementation return and fail the budget assertion.
					if fault.attempts.Load() > 2 {
						fault.failAlways.Store(false)
					}
				}
			} else {
				readFault.armed.Store(true)
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady})
			fence("only-ready")
			attempts := fault.attempts.Load()
			if mode != "bind" {
				attempts = readFault.attempts.Load()
			}
			saved, err := h.st.TurnByID(ctx, turn.ID)
			if err != nil || saved.State != domain.TurnStateQueued || saved.AgentSettings != nil || saved.ProviderTurnID != "" || saved.ProviderInputText != "" || attempts != 2 || conv.starts.Load() != 0 {
				t.Fatalf("unbounded retry or stranded queue: state=%s settings=%+v provider=%q input=%q attempts=%d starts=%d err=%v", saved.State, saved.AgentSettings, saved.ProviderTurnID, saved.ProviderInputText, attempts, conv.starts.Load(), err)
			}
			fault.failAlways.Store(false)
			readFault.armed.Store(false)
			if _, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "next real input", ClientMessageID: "next"}); err != nil {
				t.Fatal(err)
			}
			messages := conv.sentMessages()
			if len(messages) != 1 || conv.starts.Load() != 1 {
				t.Fatalf("retained input lost or duplicated: %+v starts=%d", messages, conv.starts.Load())
			}
			items := parseDeliveredBatch(t, messages[0].Text)
			if len(items) != 2 || items[0].Text != "retain waiting input" || items[1].Text != "next real input" {
				t.Fatalf("retained input changed: %+v", items)
			}
		})
	}
}
