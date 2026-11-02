package chat_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestCanceledSendSettlesPiggybackReports(t *testing.T) {
	for _, mode := range []string{"delivered", "pre-send-failed"} {
		t.Run(mode, func(t *testing.T) {
			bg := context.Background()
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			conv := &bindDeferredConversation{fakeConversation: newFakeConversation()}
			fault := &bindUnsentFaultStore{}
			pre := &queuedPreSendFaultStore{atInput: true}
			h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store { fault.Store = db; pre.Store = fault; return pre })
			worker, err := h.st.CreateSession(bg, domain.SessionRecord{ProjectID: testProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: h.now(), UpdatedAt: h.now()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.st.CreateReport(bg, domain.ReportRecord{ID: "canceled-report", SessionID: worker.ID, ProjectID: testProject, State: domain.ReportCheckpoint, Note: "canceled-worker-result", CreatedAt: h.now(), AvailableAt: h.now().Add(time.Hour), DeliveryState: domain.ReportPending, RepeatCount: 1}); err != nil {
				t.Fatal(err)
			}
			h.svc.SetReportCoordinator(reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{Store: h.st, Now: h.now, NewToken: func() string { return "canceled-token" }}))
			fault.onFault = func() {
				cancel()
				if mode == "pre-send-failed" {
					pre.armed.Store(true)
				}
			}
			fault.armed.Store(true)
			receipt, sendErr := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "accepted before disconnect", ClientMessageID: "canceled-first", Origin: domain.MessageOriginHuman})
			msg, found, err := h.st.ConversationMessageByClientID(bg, h.ctrl.ConversationID(), "canceled-first")
			if err != nil || !found {
				t.Fatalf("message: found=%v err=%v", found, err)
			}
			turn, err := h.st.TurnByID(bg, msg.TurnID)
			if err != nil {
				t.Fatal(err)
			}
			report, found, err := h.st.GetReport(bg, "canceled-report")
			if err != nil || !found {
				t.Fatalf("report: found=%v err=%v", found, err)
			}
			if mode == "pre-send-failed" {
				if receipt.State != domain.TurnStateQueued || sendErr != nil || turn.State != domain.TurnStateQueued || conv.starts.Load() != 0 || report.DeliveryState != domain.ReportAcknowledged {
					t.Errorf("canceled retained input lost report ownership: state=%s starts=%d report=%s send-error=%v", turn.State, conv.starts.Load(), report.DeliveryState, sendErr)
				}
				return
			}
			before := report.DeliveryState
			recovered, err := h.st.RequeueClaimedReports(bg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.svc.Send(bg, testSession, ports.ChatUserMessage{Text: "next real input after report recovery", ClientMessageID: "canceled-next", Origin: domain.MessageOriginHuman}); err != nil {
				t.Fatal(err)
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: turn.ProviderTurnID, TurnState: domain.TurnStateCompleted})
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: turn.ProviderTurnID, ProviderItemID: "canceled-fence", ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: "canceled-fence"})
			h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				for _, a := range s.Activities {
					if a.Summary == "canceled-fence" {
						return true
					}
				}
				return false
			})
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
			if sendErr != nil || receipt.ID != turn.ID || turn.State != domain.TurnStateRunning || before != domain.ReportAcknowledged || recovered != 0 || copies != 1 {
				t.Errorf("canceled accepted input replays reports: turn=%s starts=%d report=%s recovered=%d copies=%d send-error=%v", turn.State, conv.starts.Load(), before, recovered, copies, sendErr)
			}
		})
	}
}

func TestStaleExcerptDuringBindRecoveryReleasesReports(t *testing.T) {
	for _, mode := range []string{"direct", "queuedAhead"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			conv := &bindDeferredConversation{fakeConversation: newFakeConversation()}
			fault := &bindUnsentFaultStore{}
			changed := &changedExcerptStore{mode: "revision"}
			h := newHarnessWithConversationAndStore(t, conv, func(db *store.Store) chatsvc.Store { fault.Store = db; changed.Store = fault; return changed })
			fence := func(id string) {
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "preview-previous", ProviderItemID: id, ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: id})
				h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
					for _, a := range s.Activities {
						if a.Summary == id {
							return true
						}
					}
					return false
				})
			}
			seed, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "seed", ClientMessageID: "preview-seed"})
			if err != nil {
				t.Fatal(err)
			}
			conv.emit(ports.ChatEvent{Kind: ports.ChatEventMessageCompleted, ProviderTurnID: seed.ProviderTurnID, ProviderItemID: "preview-source", Text: "Selected source text."}, ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: seed.ProviderTurnID, TurnState: domain.TurnStateCompleted})
			snapshot := h.awaitSnapshot(t, func(s store.ConversationSnapshot) bool {
				return len(s.Messages) == 2 && s.Turns[0].State == domain.TurnStateCompleted
			})
			source := snapshot.Messages[1]
			var older domain.ConversationTurn
			if mode == "queuedAhead" {
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "preview-previous", ProviderInitiated: true})
				fence("preview-started")
				older, err = h.ctrl.Send(ctx, ports.ChatUserMessage{Text: "older queued text", ClientMessageID: "preview-older"})
				if err != nil {
					t.Fatal(err)
				}
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "preview-previous", TurnState: domain.TurnStateFailed})
				fence("preview-failed")
			}
			worker, err := h.st.CreateSession(ctx, domain.SessionRecord{ProjectID: testProject, Kind: domain.KindWorker, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: h.now(), UpdatedAt: h.now()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = h.st.CreateReport(ctx, domain.ReportRecord{ID: "preview-report", SessionID: worker.ID, ProjectID: testProject, State: domain.ReportCheckpoint, Note: "preview-worker-result", CreatedAt: h.now(), AvailableAt: h.now().Add(time.Hour), DeliveryState: domain.ReportPending, RepeatCount: 1}); err != nil {
				t.Fatal(err)
			}
			h.svc.SetReportCoordinator(reportsvc.NewCoordinator(reportsvc.CoordinatorDeps{Store: h.st, Now: h.now, NewToken: func() string { return "preview-token" }}))
			fault.onFault = func() { changed.changed.Store(true) }
			fault.armed.Store(true)
			receipt, firstErr := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "refer to this source", ClientMessageID: "preview-stale", Origin: domain.MessageOriginHuman, Excerpts: []ports.ChatExcerptReference{{ConversationID: h.ctrl.ConversationID(), MessageID: source.ID, Revision: source.Revision, Text: "Selected source"}}})
			saved, err := h.st.TurnByID(ctx, receipt.ID)
			if err != nil {
				t.Fatal(err)
			}
			report, found, err := h.st.GetReport(ctx, "preview-report")
			if err != nil || !found {
				t.Fatalf("report: found=%v err=%v", found, err)
			}
			if _, err := h.svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "next valid input", ClientMessageID: "preview-next", Origin: domain.MessageOriginHuman}); err != nil {
				t.Fatal(err)
			}
			if mode == "queuedAhead" {
				running, err := h.st.TurnByID(ctx, older.ID)
				if err != nil {
					t.Fatal(err)
				}
				conv.emit(ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: running.ProviderTurnID, TurnState: domain.TurnStateCompleted})
			}
			fence("preview-delivered")
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
			if receipt.State != domain.TurnStateFailed || saved.State != domain.TurnStateFailed || firstErr == nil || report.DeliveryState != domain.ReportPending || copies != 1 {
				t.Errorf("preview pre-send failure consumed report: receipt=%s durable=%s error=%v report=%s copies=%d", receipt.State, saved.State, firstErr, report.DeliveryState, copies)
			}
		})
	}
}
