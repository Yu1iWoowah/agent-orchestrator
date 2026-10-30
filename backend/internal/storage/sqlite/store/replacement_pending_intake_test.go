package store_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	fixturestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type replacementPendingReader interface {
	ReadReplacementIntake(context.Context, string) (domain.ReplacementIntake, error)
}

func TestReplacementPendingIntakeIncludesPostCutoffTUIAndReportsWithoutMutation(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seedProject(t, st, "replace")
	source := sampleRecord("replace")
	source.Kind, source.Mode = domain.KindOrchestrator, domain.SessionModeTUI
	source.Metadata.RuntimeLaunchID = "launch"
	source.Metadata.AgentSessionID = "native-tui"
	source.Metadata.AgentSessionIDLaunchID = "launch"
	source.Metadata.BrowserCapabilityVerifier = "source-capability-verifier"
	source, err := st.CreateSession(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	acceptedAt := time.Now().UTC()
	intent := domain.OrchestratorReplacement{SchemaVersion: 1, ID: "11111111-1111-4111-8111-111111111111", OperationKey: "22222222-2222-4222-8222-222222222222", RequestHash: strings.Repeat("a", 64), ProjectID: source.ProjectID, SourceSessionID: source.ID, SourceOwner: source.ControllerOwner(), SourceCapabilityVerifier: source.Metadata.BrowserCapabilityVerifier, Phase: domain.ReplacementAccepted, CreatedAt: acceptedAt, UpdatedAt: acceptedAt, DeadlineAt: acceptedAt.Add(time.Hour)}
	op, _, err := st.CreateOrchestratorReplacement(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	op.Phase = domain.ReplacementExportingTranscript
	op.FrozenAt = &now
	op.Transcript = &domain.ReplacementTranscript{AttemptID: "33333333-3333-4333-8333-333333333333", AO: domain.TranscriptSourceCutoff{ConversationCutoffs: map[string]int64{}}}
	op.Preparation = &domain.ReplacementPreparation{ControlClientID: "preparation-control"}
	op = fixturestore.SeedReplacementTestFacts(t, st, op)
	conversation, err := st.CreateConversation(ctx, "source-tui-pending", domain.ConversationScopeSession, op.ProjectID, op.SourceSessionID, now)
	if err != nil {
		t.Fatal(err)
	}
	payload := `[{"type":"image","fileReference":{"sessionId":"original-source","name":"queued.png"}},{"type":"file","fileReference":{"sessionId":"original-source","name":"pending.pdf"}}]`
	for _, id := range []string{"preparation-control", "late-A-Q22"} {
		if added, err := st.AppendUserMessage(ctx, conversation.ID, op.SourceSessionID, "", domain.ConversationMessage{ID: id, ClientMessageID: id, Text: "A-Q22: B; retain D-21", Origin: domain.MessageOriginHuman, DeliveryContentJSON: payload, AuthoredByUser: true}, id, now); err != nil || !added {
			t.Fatal(added, err)
		}
	}
	worker, err := st.CreateSession(ctx, sampleRecord(string(op.ProjectID)))
	if err != nil {
		t.Fatal(err)
	}
	report := domain.ReportRecord{ID: "open-Q23", SessionID: worker.ID, ProjectID: op.ProjectID, State: domain.ReportNeedsInput, Note: "Q23: which boundary?", CreatedAt: now, AvailableAt: now, DeliveryState: domain.ReportPending, RepeatCount: 1}
	if _, err := st.CreateReport(ctx, report); err != nil {
		t.Fatal(err)
	}
	before, _, err := st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	reader, ok := any(st).(replacementPendingReader)
	if !ok {
		t.Fatal("live pending intake reader unavailable; export cutoff cannot expose late TUI input")
	}
	result, err := reader.ReadReplacementIntake(ctx, op.ID)
	if err != nil || len(result.Messages) != 1 || len(result.Reports) != 1 {
		t.Fatal("pending inventory omitted late intake", result, err)
	}
	message := result.Messages[0]
	if message.ID != "late-A-Q22" || message.ClientMessageID != "late-A-Q22" || message.DeliveryContentJSON != payload || message.NativeDelivery != "unconfirmed" || message.ProviderReserved || message.ConversationID != conversation.ID {
		t.Fatal("pending message lost receipt/payload or claimed delivery", message)
	}
	if result.Reports[0].ID != report.ID || result.Reports[0].Note != report.Note || result.Reports[0].DeliveryState != domain.ReportPending {
		t.Fatal("pending report lost question", result.Reports)
	}
	after, _, err := st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("read changed operation", err)
	}
	persisted, _, err := st.GetReport(ctx, report.ID)
	if err != nil || persisted.DeliveryState != domain.ReportPending || persisted.ClaimToken != "" {
		t.Fatal("read claimed report", err)
	}
	again, err := reader.ReadReplacementIntake(ctx, op.ID)
	if err != nil || !reflect.DeepEqual(result, again) {
		t.Fatal("same facts did not reread identically", err)
	}
}

func TestReplacementPendingIntakeKeepsReservationsAndReportClaimsPrivate(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	intent := replacementOperation(t, st)
	now := time.Now().UTC()
	conv, err := st.CreateConversation(ctx, "source-live-queue", domain.ConversationScopeProject, intent.ProjectID, intent.SourceSessionID, now)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := st.AppendUserMessage(ctx, conv.ID, intent.SourceSessionID, intent.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: "reserved-message", ClientMessageID: "reserved-message", Text: "preserve reserved input", Origin: domain.MessageOriginHuman}, "reserved-turn", now); err != nil || !added {
		t.Fatal(added, err)
	}
	if batch, err := st.ClaimQueuedBatch(ctx, conv.ID, false, 1, now); err != nil || len(batch) != 1 {
		t.Fatal(batch, err)
	}
	op, _, err := st.CreateOrchestratorReplacement(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	workerSeed := sampleRecord(string(op.ProjectID))
	workerSeed.ProvisionState = domain.SessionProvisionProvisioning
	worker, err := st.CreateSession(ctx, workerSeed)
	if err != nil {
		t.Fatal(err)
	}
	private, err := st.CreateConversation(ctx, "worker-private-queue", domain.ConversationScopeSession, op.ProjectID, worker.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if added, err := st.AppendUserMessage(ctx, private.ID, worker.ID, "", domain.ConversationMessage{ID: "worker-secret", Text: "worker private queue", Origin: domain.MessageOriginHuman}, "worker-turn", now); err != nil || !added {
		t.Fatal(added, err)
	}
	for _, id := range []string{"claimed-report", "acknowledged-report"} {
		report := domain.ReportRecord{ID: id, SessionID: worker.ID, ProjectID: op.ProjectID, State: domain.ReportNeedsInput, Note: "Q24: pending boundary", CreatedAt: now, AvailableAt: now, DeliveryState: domain.ReportPending, RepeatCount: 1, Outputs: []domain.ReportOutput{{Kind: domain.ReportOutputArtifact, Reference: "/retained/evidence.md"}}}
		if _, err := st.CreateReport(ctx, report); err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := st.ClaimReport(ctx, id, "private-claim-token", now); err != nil || !claimed {
			t.Fatal(claimed, err)
		}
		if id == "acknowledged-report" {
			if _, applied, err := st.AcknowledgeReport(ctx, id, "private-claim-token", now); err != nil || !applied {
				t.Fatal(applied, err)
			}
		}
	}
	before, _, err := st.GetReport(ctx, "claimed-report")
	if err != nil {
		t.Fatal(err)
	}
	result, err := pendingIntakeAPI(t, st).ReadReplacementIntake(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Messages) != 1 || !result.Messages[0].ProviderReserved || result.Messages[0].NativeDelivery != "unconfirmed" || len(result.Reports) != 1 || result.Reports[0].ID != "claimed-report" || len(result.Reports[0].Outputs) != 1 {
		t.Fatal("reservation/claim/read scopes confused", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), "private-claim-token") || strings.Contains(string(encoded), "worker-secret") || strings.Contains(string(encoded), "acknowledged-report") {
		t.Fatal("pending inventory leaked internal/unrelated data", string(encoded), err)
	}
	after, _, err := st.GetReport(ctx, "claimed-report")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("read changed existing report claim", err)
	}
}

func TestReplacementPendingIntakeExcludesArchivedControlsAcrossOperations(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	intent := replacementOperation(t, st)
	now := time.Now().UTC()
	op, _, err := st.CreateOrchestratorReplacement(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = domain.ReplacementPreparingHandoff
	op.Preparation = &domain.ReplacementPreparation{AttemptID: "33333333-3333-4333-8333-333333333333", LeaseEpoch: 7, ControlClientID: "archived-control"}
	op = fixturestore.SeedReplacementTestFacts(t, st, op)
	op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, applied, err := st.ResetReplacementBeforeStop(ctx, op.ID, op.Revision, true, now, now.Add(time.Hour)); err != nil || !applied {
		t.Fatal(applied, err)
	}
	intent.ID = "44444444-4444-4444-8444-444444444444"
	intent.OperationKey = "55555555-5555-4555-8555-555555555555"
	next, _, err := st.CreateOrchestratorReplacement(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := st.CreateConversation(ctx, "next-attempt-queue", domain.ConversationScopeProject, next.ProjectID, next.SourceSessionID, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"archived-control", "new-human-instruction"} {
		if added, err := st.AppendUserMessage(ctx, conv.ID, next.SourceSessionID, next.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: id, ClientMessageID: id, Text: id, Origin: domain.MessageOriginHuman}, id, now); err != nil || !added {
			t.Fatal(added, err)
		}
	}
	got, err := pendingIntakeAPI(t, st).ReadReplacementIntake(ctx, next.ID)
	if err != nil || len(got.Messages) != 1 || got.Messages[0].ID != "new-human-instruction" {
		t.Fatal("old control was interpreted as new pending input", got, err)
	}
}

func pendingIntakeAPI(t *testing.T, st any) replacementPendingReader {
	t.Helper()
	api, ok := st.(replacementPendingReader)
	if !ok {
		t.Fatal("live pending intake reader unavailable")
	}
	return api
}

func TestReplacementPendingIntakeMarksAnyPriorDispatchEvidenceUnconfirmed(t *testing.T) {
	for _, marker := range []string{"provider", "input", "started", "completed", "batched"} {
		t.Run(marker, func(t *testing.T) {
			st, _, op, raw := repairFixture(t)
			ctx := context.Background()
			conv, err := st.CreateConversation(ctx, "uncertain-intake", domain.ConversationScopeProject, op.ProjectID, op.SourceSessionID, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.AppendUserMessage(ctx, conv.ID, op.SourceSessionID, op.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: "uncertain-message", Text: "retain uncertain delivery", Origin: domain.MessageOriginHuman}, "uncertain-turn", time.Now()); err != nil {
				t.Fatal(err)
			}
			statements := map[string]string{
				"provider":  `UPDATE conversation_turns SET provider_turn_id='unknown-provider' WHERE id='uncertain-turn'`,
				"input":     `UPDATE conversation_turns SET provider_input_text='possibly dispatched' WHERE id='uncertain-turn'`,
				"started":   `UPDATE conversation_turns SET started_at=CURRENT_TIMESTAMP WHERE id='uncertain-turn'`,
				"completed": `UPDATE conversation_turns SET completed_at=CURRENT_TIMESTAMP WHERE id='uncertain-turn'`,
				"batched":   `UPDATE conversation_turns SET batched_input=1 WHERE id='uncertain-turn'`,
			}
			if _, err := raw.Exec(statements[marker]); err != nil {
				t.Fatal(err)
			}
			result, err := pendingIntakeAPI(t, st).ReadReplacementIntake(ctx, op.ID)
			if err != nil || len(result.Messages) != 1 || !result.Messages[0].ProviderReserved || result.Messages[0].NativeDelivery != "unconfirmed" {
				t.Fatal("uncertain evidence hidden or called delivered", marker, result, err)
			}
		})
	}
}
