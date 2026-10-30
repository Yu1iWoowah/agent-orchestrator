package chat_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestReplacementQuiescencePreservesHeldQueueAndFencesChangedOperation(t *testing.T) {
	for _, which := range []string{"idle", "background", "running", "changed operation", "changed capability", "missing intent"} {
		t.Run(which, func(t *testing.T) {
			h, provider := hibernationHarnessForKind(t, domain.TurnStateCompleted, domain.KindOrchestrator, func(opts *chatsvc.Options) { opts.HibernationEnabled = func() bool { return false } })
			ctx := context.Background()
			rec, _, err := h.st.GetSession(ctx, testSession)
			if err != nil {
				t.Fatal(err)
			}
			rec.Kind, rec.Harness = domain.KindOrchestrator, domain.HarnessClaudeCode
			rec.Metadata.BrowserCapabilityVerifier = "verified-source-capability"
			if err := h.st.UpdateSession(ctx, rec); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			op, _, err := h.st.CreateOrchestratorReplacement(ctx, domain.OrchestratorReplacement{SchemaVersion: 1, ID: "11111111-1111-4111-8111-111111111111", OperationKey: "22222222-2222-4222-8222-222222222222", RequestHash: strings.Repeat("a", 64), ProjectID: rec.ProjectID, SourceSessionID: rec.ID, SourceOwner: rec.ControllerOwner(), SourceCapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, Phase: domain.ReplacementAccepted, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			op.FrozenAt = &now
			if which != "missing intent" {
				op.SourceStopIntentAt = &now
			}
			op.Phase = domain.ReplacementExportingTranscript
			op.Preparation = &domain.ReplacementPreparation{VerifiedAt: &now}
			op.Transcript = &domain.ReplacementTranscript{AttemptID: "33333333-3333-4333-8333-333333333333", AO: domain.TranscriptSourceCutoff{ConversationCutoffs: map[string]int64{}}}
			project, _, err := h.st.GetProject(ctx, string(rec.ProjectID))
			if err != nil {
				t.Fatal(err)
			}
			op = sqlitetest.ReplacementPriorFacts(op)
			op = sqlitetest.SeedReplacement(t, project.Path, op)
			op, _, err = h.st.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := h.svc.Snapshot(ctx, testSession)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Conversation.Scope != domain.ConversationScopeProject {
				t.Fatal("fixture did not open project conversation")
			}
			image := `[{"type":"image","data":"aW1hZ2U=","mimeType":"image/png"}]`
			if _, err := h.st.AppendUserMessage(ctx, snapshot.Conversation.ID, testSession, rec.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "held", ClientMessageID: "held", Text: "keep original receipt", Origin: domain.MessageOriginHuman, DeliveryContentJSON: image}, "held-turn", now); err != nil {
				t.Fatal(err)
			}
			switch which {
			case "background":
				provider.backgroundRunning = true
			case "running":
				if err := h.st.BindTurnToProvider(ctx, snapshot.Turns[0].ID, "another-running-turn", now); err != nil {
					t.Fatal(err)
				}
			case "changed operation":
				provider.onBackgroundCheck = func(context.Context) {
					op.Phase, op.ResumePhase = domain.ReplacementRecoveryRequired, domain.ReplacementExportingTranscript
					if ok, err := h.st.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
						t.Fatal(ok, err)
					}
				}
			case "changed capability":
				provider.onBackgroundCheck = func(context.Context) {
					current, _, err := h.st.GetSession(ctx, testSession)
					if err != nil {
						t.Fatal(err)
					}
					current.Metadata.BrowserCapabilityVerifier = "rotated"
					if err := h.st.UpdateSession(ctx, current); err != nil {
						t.Fatal(err)
					}
				}
			}
			if which == "idle" {
				if stopped, err := h.svc.HibernateChatForUpdate(ctx, testSession); err != nil || stopped || provider.calls.Load() != 0 {
					t.Fatalf("unbound update stop bypassed replacement: stopped=%t err=%v", stopped, err)
				}
			}
			stopped, err := h.svc.HibernateChatForReplacement(ctx, testSession, op.ID)
			if which == "idle" {
				if err != nil || !stopped || provider.calls.Load() != 1 {
					current, _, _ := h.st.GetSession(ctx, testSession)
					latest, _ := h.st.LatestVisibleUserTurnSettled(ctx, snapshot.Conversation.ID, testSession)
					queued, qe := h.st.NextQueuedTurn(ctx, snapshot.Conversation.ID)
					t.Fatalf("verified idle source was not parked: stopped=%t calls=%d err=%v allows=%t activity=%+v latest=%t queued=%+v qe=%v conv=%+v", stopped, provider.calls.Load(), err, op.AllowsChatQuiescence(current), current.Activity, latest, queued, qe, snapshot.Conversation)
				}
				var wakes int
				h.svc.SetWakeCallback(func(context.Context, domain.SessionID) error { wakes++; return nil })
				if err := h.svc.SetChatView(ctx, testSession, "replacement-read-only-view", true); err != nil {
					t.Fatal(err)
				}
				if wakes != 0 {
					t.Fatal("read-only view woke the fenced source provider")
				}
			} else if stopped || provider.calls.Load() != 0 {
				t.Fatalf("unverified source stopped: %t calls=%d err=%v", stopped, provider.calls.Load(), err)
			}
			rows, err := h.st.LoadConversationSnapshot(ctx, snapshot.Conversation.ID)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, message := range rows.Messages {
				if message.ID == "held" {
					found = true
					if message.DeliveryContentJSON != image {
						t.Fatal("attachment changed")
					}
				}
			}
			for _, turn := range rows.Turns {
				if turn.ID == "held-turn" && turn.State != domain.TurnStateQueued {
					t.Fatal("held turn consumed", turn.State)
				}
			}
			current, _, err := h.st.GetSession(ctx, testSession)
			if err != nil || current.IsTerminated || !found {
				t.Fatal("source/queued receipt lost", err)
			}
		})
	}
}

func (c *hibernationConversation) HibernateForReplacement() error { return c.Hibernate() }
