package chat_test

import (
	"context"
	"errors"
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

type queuedPreSendFaultStore struct {
	chatsvc.Store
	armed           atomic.Bool
	atInput         bool
	invalidPreview  atomic.Bool
	invalidClientID string
}

func (s *queuedPreSendFaultStore) RecordTurnSettingsSnapshot(ctx context.Context, cid, generation string, ids []string, snapshot domain.AgentSettingsTurnSnapshot) error {
	if !s.atInput && s.armed.CompareAndSwap(true, false) {
		return errors.New("queued-settings-injected")
	}
	return s.Store.RecordTurnSettingsSnapshot(ctx, cid, generation, ids, snapshot)
}

func (s *queuedPreSendFaultStore) RecordProviderInput(ctx context.Context, id, text string) error {
	if s.atInput && s.armed.CompareAndSwap(true, false) {
		return errors.New("queued-provider-input-injected")
	}
	return s.Store.RecordProviderInput(ctx, id, text)
}

func (s *queuedPreSendFaultStore) ListQueuedBatch(ctx context.Context, cid string, limit int) ([]domain.QueuedTurn, error) {
	batch, err := s.Store.ListQueuedBatch(ctx, cid, limit)
	if err == nil && s.invalidPreview.CompareAndSwap(true, false) {
		for i := range batch {
			if batch[i].ClientMessageID == s.invalidClientID {
				batch[i].DeliveryContentJSON = "{"
			}
		}
	}
	return batch, err
}

type uncertainDeferredConversation struct {
	*bindDeferredConversation
	uncertain atomic.Bool
}

func (c *uncertainDeferredConversation) SendTurn(ctx context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	if c.uncertain.CompareAndSwap(true, false) {
		c.mu.Lock()
		c.sent = append(c.sent, msg)
		c.mu.Unlock()
		return ports.ChatTurnRef{}, errors.New("provider accepted input but response was lost")
	}
	return c.bindDeferredConversation.SendTurn(ctx, msg)
}

func TestDispatchRecoveryPreservesPiggybackReports(t *testing.T) {
	for _, mode := range []string{"direct", "queuedAhead", "queuedList", "queuedClaim", "settings", "input", "direct-settings", "direct-input", "uncertain", "bindRequeue", "invalidPreview", "invalidPreviewRetry", "invalidPreviewOlderUncertain"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			direct := strings.HasPrefix(mode, "direct")
			invalidPreview := strings.HasPrefix(mode, "invalidPreview")
			previewRejectsCurrent := invalidPreview && mode != "invalidPreviewOlderUncertain"
			preSendFailure := strings.HasSuffix(mode, "settings") || strings.HasSuffix(mode, "input")
			rejectedUnsent := mode == "bindRequeue" || previewRejectsCurrent
			conv := &uncertainDeferredConversation{bindDeferredConversation: &bindDeferredConversation{fakeConversation: newFakeConversation()}}
			fault := &bindUnsentFaultStore{}
			preSendFault := &queuedPreSendFaultStore{atInput: strings.HasSuffix(mode, "input"), invalidClientID: "accepted"}
			if mode == "invalidPreviewOlderUncertain" {
				preSendFault.invalidClientID = "older"
			}
			readFault := &readyRetryFaultStore{atClaim: mode == "queuedClaim", persistent: true, requeued: make(chan struct{})}
			h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store {
				fault.Store = db
				if preSendFailure || invalidPreview {
					preSendFault.Store = fault
					return preSendFault
				}
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
			if !direct {
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
			if _, err = h.st.CreateReport(ctx, domain.ReportRecord{ID: "queued-report", SessionID: worker.ID, ProjectID: testProject, State: domain.ReportCheckpoint, Note: "queued-report-note", CreatedAt: h.now(), AvailableAt: h.now().Add(time.Hour), DeliveryState: domain.ReportPending, RepeatCount: 1}); err != nil {
				t.Fatal(err)
			}
			h.svc.SetReportCoordinator(reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{Store: h.st, Now: h.now, NewToken: func() string { return "queued-claim" }}))
			if mode == "queuedList" || mode == "queuedClaim" {
				readFault.armed.Store(true)
			} else if preSendFailure {
				if direct {
					fault.onFault = func() { preSendFault.armed.Store(true) }
					fault.armed.Store(true)
				} else {
					preSendFault.armed.Store(true)
				}
			} else if mode == "bindRequeue" {
				fault.requeueErr = errors.New("queued-requeue-injected")
				fault.armed.Store(true)
			} else if invalidPreview {
				preSendFault.invalidPreview.Store(true)
				switch mode {
				case "invalidPreviewRetry":
					fault.armed.Store(true)
				case "invalidPreviewOlderUncertain":
					conv.uncertain.Store(true)
				}
			} else if mode == "uncertain" {
				conv.uncertain.Store(true)
			} else {
				fault.armed.Store(true)
			}
			turn, firstErr := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "accepted input", ClientMessageID: "accepted", Origin: domain.MessageOriginHuman})
			saved, err := h.st.TurnByID(ctx, turn.ID)
			if err != nil {
				t.Fatal(err)
			}
			report, found, err := h.st.GetReport(ctx, "queued-report")
			if err != nil || !found {
				t.Fatalf("report: found=%v err=%v", found, err)
			}
			if rejectedUnsent {
				wantStarts := int32(0)
				if previewRejectsCurrent {
					wantStarts = 1 // The older, valid input must still dispatch.
				}
				if firstErr == nil || saved.State != domain.TurnStateFailed || saved.ProviderTurnID != "" || report.DeliveryState != domain.ReportPending || conv.starts.Load() != wantStarts {
					t.Errorf("unsent failed message consumed reports: err=%v turn=%+v report=%s starts=%d", firstErr, saved, report.DeliveryState, conv.starts.Load())
				}
			} else if firstErr != nil || report.DeliveryState != domain.ReportAcknowledged {
				t.Errorf("retained message released reports: err=%v state=%s report=%s", firstErr, saved.State, report.DeliveryState)
			}
			if mode == "queuedList" || mode == "queuedClaim" {
				if readFault.attempts.Load() != 2 {
					t.Errorf("queue store retry was not bounded to two attempts: %d", readFault.attempts.Load())
				}
				readFault.armed.Store(false)
			}
			if previewRejectsCurrent {
				messages := conv.sentMessages()
				if len(messages) != 1 || messages[0].Text != "older queued input" {
					t.Fatalf("invalid preview blocked or corrupted valid input: %+v", messages)
				}
				older, found, err := h.st.ConversationMessageByClientID(ctx, h.ctrl.ConversationID(), "older")
				if err != nil || !found {
					t.Fatalf("valid queued message: found=%t err=%v", found, err)
				}
				olderTurn, err := h.st.TurnByID(ctx, older.TurnID)
				if err != nil || olderTurn.ProviderTurnID == "" {
					t.Fatalf("valid input did not bind: %+v err=%v", olderTurn, err)
				}
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: olderTurn.ProviderTurnID, TurnState: domain.TurnStateCompleted})
				fence("valid-preview-input-completed")
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
			if copies != 1 {
				t.Errorf("durably accepted message rejected: err=%v report-after-first=%s delivered-report-copies=%d", firstErr, report.DeliveryState, copies)
			}
		})
	}
}

func TestDirectBindFailureSameIDRetryDoesNotDuplicateInput(t *testing.T) {
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

type readyBusyBindConversation struct {
	*bindDeferredConversation
	refuse atomic.Bool
}

func (c *readyBusyBindConversation) SendTurn(ctx context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	if c.refuse.CompareAndSwap(true, false) {
		return ports.ChatTurnRef{}, ports.ErrChatTurnNotSentBusy
	}
	return c.bindDeferredConversation.SendTurn(ctx, msg)
}
func TestLastReadyBindFailureRecoversWithoutAnotherReady(t *testing.T) {
	ctx := context.Background()
	conv := &readyBusyBindConversation{bindDeferredConversation: &bindDeferredConversation{fakeConversation: newFakeConversation()}}
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
			conv := &readyBusyBindConversation{bindDeferredConversation: &bindDeferredConversation{fakeConversation: newFakeConversation()}}
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
