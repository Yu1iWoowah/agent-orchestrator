package chat_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
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
	armed      atomic.Bool
	failAlways atomic.Bool
	attempts   atomic.Int32
	onFault    func()
	requeueErr error
}

var errBindUnsent = errors.New("bind-unsent-injected")

func (s *bindUnsentFaultStore) BindTurnToProvider(ctx context.Context, id, pid string, at time.Time) error {
	s.attempts.Add(1)
	if s.armed.CompareAndSwap(true, false) || s.failAlways.Load() {
		if s.onFault != nil {
			s.onFault()
		}
		return errBindUnsent
	}
	return s.Store.BindTurnToProvider(ctx, id, pid, at)
}

func (s *bindUnsentFaultStore) RequeueUnsentTurn(ctx context.Context, cid, id, pid string, followers []string) error {
	if s.requeueErr != nil {
		return s.requeueErr
	}
	return s.Store.RequeueUnsentTurn(ctx, cid, id, pid, followers)
}

func TestDeferredDirectBindFailureReturnsRequeueError(t *testing.T) {
	ctx := context.Background()
	conv := &bindDeferredConversation{fakeConversation: newFakeConversation()}
	requeueErr := errors.New("direct-requeue-injected")
	fault := &bindUnsentFaultStore{requeueErr: requeueErr}
	h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store {
		fault.Store = db
		return fault
	})
	fault.armed.Store(true)
	_, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "do not hide a failed requeue", ClientMessageID: "requeue-error"})
	if !errors.Is(err, errBindUnsent) || !errors.Is(err, requeueErr) || fault.attempts.Load() != 1 || conv.starts.Load() != 0 {
		t.Fatalf("requeue failure accepted/retried: err=%v binds=%d starts=%d", err, fault.attempts.Load(), conv.starts.Load())
	}
}

func TestDeferredDirectBindFailureRetriesWithoutReady(t *testing.T) {
	ctx := context.Background()
	conv := &bindDeferredConversation{fakeConversation: newFakeConversation()}
	fault := &bindUnsentFaultStore{}
	h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store {
		fault.Store = db
		return fault
	})
	// A different capture time also proves the unsent snapshot was cleared.
	fault.onFault = func() { h.advance(time.Second) }
	fault.armed.Store(true)
	msg := ports.ChatUserMessage{Text: "recover without a provider Ready", ClientMessageID: "direct"}
	turn, err := h.ctrl.Send(ctx, msg)
	if err != nil || turn.ID == "" || turn.State != domain.TurnStateQueued || fault.attempts.Load() != 2 || conv.starts.Load() != 1 {
		t.Fatalf("direct recovery: receipt=%+v err=%v binds=%d starts=%d", turn, err, fault.attempts.Load(), conv.starts.Load())
	}
	saved, err := h.st.TurnByID(ctx, turn.ID)
	if err != nil || saved.State != domain.TurnStateRunning || saved.AgentSettings == nil || saved.AgentSettings.CapturedAt == nil || !saved.AgentSettings.CapturedAt.Equal(h.now()) {
		t.Fatalf("recovered input/snapshot: %+v %v", saved, err)
	}
	if _, err := h.ctrl.Send(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if messages := conv.sentMessages(); len(messages) != 1 || messages[0].Text != msg.Text || conv.discards.Load() != 1 {
		t.Fatalf("recovery lost or duplicated input: %+v discards=%d", messages, conv.discards.Load())
	}
}

func TestDeferredDirectBindFailureRecoversAfterRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conv := &bindDeferredConversation{fakeConversation: newFakeConversation()}
	fault := &bindUnsentFaultStore{onFault: cancel}
	h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store {
		fault.Store = db
		return fault
	})
	fault.armed.Store(true)
	msg := ports.ChatUserMessage{Text: "recover after client disconnects", ClientMessageID: "canceled-direct"}
	turn, err := h.ctrl.Send(ctx, msg)
	if ctx.Err() != context.Canceled || err != nil || turn.ID == "" || turn.State != domain.TurnStateQueued || fault.attempts.Load() != 2 || conv.starts.Load() != 1 {
		t.Fatalf("canceled direct recovery: ctx=%v receipt=%+v err=%v binds=%d starts=%d", ctx.Err(), turn, err, fault.attempts.Load(), conv.starts.Load())
	}
	saved, err := h.st.TurnByID(context.Background(), turn.ID)
	if err != nil || saved.State != domain.TurnStateRunning {
		t.Fatalf("accepted input stayed queued after cancellation: %+v %v", saved, err)
	}
	if _, err := h.ctrl.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if messages := conv.sentMessages(); len(messages) != 1 || messages[0].Text != msg.Text || conv.starts.Load() != 1 || conv.discards.Load() != 1 {
		t.Fatalf("canceled request lost or duplicated input: %+v starts=%d discards=%d", messages, conv.starts.Load(), conv.discards.Load())
	}
}

func TestDeferredDirectBindFailureAcceptsPiggybackWithoutDuplication(t *testing.T) {
	ctx := context.Background()
	conv := &bindDeferredConversation{fakeConversation: newFakeConversation()}
	fault := &bindUnsentFaultStore{}
	h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store {
		fault.Store = db
		return fault
	})
	worker, err := h.st.CreateSession(ctx, domain.SessionRecord{
		ProjectID: testProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex,
		Mode: domain.SessionModeChat, CreatedAt: h.now(), UpdatedAt: h.now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	const proof = "unique worker result retained by this message"
	if _, err := h.st.CreateReport(ctx, domain.ReportRecord{
		ID: "direct-report", SessionID: worker.ID, ProjectID: testProject,
		State: domain.ReportCheckpoint, Note: proof, CreatedAt: h.now(),
		AvailableAt: h.now().Add(time.Hour), DeliveryState: domain.ReportPending, RepeatCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	h.svc.SetReportCoordinator(reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{
		Store: h.st, Now: h.now, NewToken: func() string { return "direct-claim" },
	}))
	fault.failAlways.Store(true)
	fault.onFault = func() {
		if fault.attempts.Load() > 2 {
			t.Fatal("direct recovery retried a persistent Bind failure more than once")
		}
	}
	first, sendErr := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "first request", ClientMessageID: "first"})
	report, found, err := h.st.GetReport(ctx, "direct-report")
	if err != nil || !found || report.DeliveryState != domain.ReportAcknowledged {
		t.Fatalf("retained message released its report: report=%+v found=%t err=%v sendErr=%v", report, found, err, sendErr)
	}
	if sendErr != nil || first.ID == "" || first.State != domain.TurnStateQueued || fault.attempts.Load() != 2 || conv.starts.Load() != 0 {
		t.Fatalf("bounded direct receipt: %+v err=%v binds=%d starts=%d", first, sendErr, fault.attempts.Load(), conv.starts.Load())
	}
	saved, err := h.st.TurnByID(ctx, first.ID)
	if err != nil || saved.State != domain.TurnStateQueued || saved.AgentSettings != nil || saved.ProviderInputText != "" {
		t.Fatalf("persistent Bind failure stranded input: %+v %v", saved, err)
	}
	// The next real user message drains the retained request and must not claim
	// the same report again. No synthetic provider Ready is emitted in this test.
	fault.failAlways.Store(false)
	h.advance(time.Second)
	second, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "next request", ClientMessageID: "next"})
	if err != nil || second.ID == "" || second.State != domain.TurnStateQueued {
		t.Fatalf("next send: %+v %v", second, err)
	}
	messages := conv.sentMessages()
	if len(messages) != 1 || strings.Count(messages[0].Text, proof) != 1 || conv.starts.Load() != 1 {
		t.Fatalf("worker report lost or duplicated: %+v starts=%d", messages, conv.starts.Load())
	}
	items := parseDeliveredBatch(t, messages[0].Text)
	if len(items) != 2 || !strings.Contains(items[0].Text, proof) || items[1].Text != "next request" {
		t.Fatalf("report was appended to the next message again: %+v", items)
	}
}

func TestDeferredBindFailurePreservesUnsentInput(t *testing.T) {
	for _, mode := range []string{"queued", "batch"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			conv := &bindDeferredConversation{fakeConversation: newFakeConversation()}
			fault := &bindUnsentFaultStore{}
			h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store {
				fault.Store = db
				return fault
			})
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "previous", ProviderInitiated: true})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool { return len(s.Turns) == 1 })
			fault.failAlways.Store(true)
			texts := []string{"keep lead"}
			if mode == "batch" {
				texts = append(texts, "keep follower")
			}
			for _, text := range texts {
				turn, err := h.ctrl.Send(ctx, ports.ChatUserMessage{Text: text, ClientMessageID: text})
				if err != nil || turn.ID == "" || turn.State != domain.TurnStateQueued {
					t.Fatalf("Send: %+v %v", turn, err)
				}
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "previous", TurnState: domain.TurnStateCompleted})
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
			if conv.starts.Load() != 0 || conv.discards.Load() != 2 {
				t.Fatalf("failed binding crossed provider boundary: starts=%d discards=%d", conv.starts.Load(), conv.discards.Load())
			}
			fault.failAlways.Store(false)
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
