package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	fixturestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type intakeReleaseStore interface {
	ReleaseReplacementIntake(context.Context, string, int64, time.Time) (domain.OrchestratorReplacement, bool, error)
	ReplacementExcerptSourceAllowed(context.Context, domain.SessionID, string, string, string, string, string) (bool, error)
}

type intakeTransferFixture struct {
	st             *sqlite.Store
	api            intakeReleaseStore
	op             domain.OrchestratorReplacement
	source, target domain.ConversationRecord
	writer, worker domain.SessionRecord
	raw            *sql.DB
	payload        string
}

// Only fixture facts cross the 3C boundary: no runtime stop, resume or writer
// activation producer is delivered by 3B. The normal save API cannot mint them.
func newIntakeTransferFixture(t *testing.T) intakeTransferFixture {
	return newIntakeTransferFixtureWithPromotion(t, false)
}

func newIntakeTransferFixtureWithPromotion(t *testing.T, promoting bool) intakeTransferFixture {
	return newCanonicalIntakeFixture(t, promoting, true)
}

func newCanonicalIntakeFixture(t *testing.T, promoting, activated bool) intakeTransferFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st := sqlitetest.MustOpenAt(t, dir)
	raw, err := sql.Open("sqlite", filepath.Join(dir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	op := replacementOperation(t, st)
	project, _, err := st.GetProject(ctx, string(op.ProjectID))
	if err != nil {
		t.Fatal(err)
	}
	op.ProjectFingerprint, err = domain.ReplacementProjectFingerprint(project)
	if err != nil {
		t.Fatal(err)
	}
	source, err := st.CreateConversation(ctx, "original-source-queue", domain.ConversationScopeSession, op.ProjectID, op.SourceSessionID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if _, err := st.AppendUserMessage(ctx, source.ID, op.SourceSessionID, op.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: "history-question", Text: "original question", Origin: domain.MessageOriginHuman}, "history-turn", at); err != nil {
		t.Fatal(err)
	}
	if err := st.BindTurnToProvider(ctx, "history-turn", "history-provider", at); err != nil {
		t.Fatal(err)
	}
	if err := st.SettleAssistantMessage(ctx, source.ID, "history-item", "history-provider", "historical selected answer", "history-answer", at); err != nil {
		t.Fatal(err)
	}
	if err := st.SettleTurnByID(ctx, "history-turn", domain.TurnStateCompleted, "", at); err != nil {
		t.Fatal(err)
	}
	if !promoting {
		op, _, err = st.CreateOrchestratorReplacement(ctx, op)
		if err != nil {
			t.Fatal(err)
		}
	}
	payload := `[{"type":"image","mimeType":"image/png","data":"aW1hZ2U="},{"type":"resource","uri":"artifact://original-reference","mimeType":"text/plain"},{"type":"excerpt","excerpt":{"reference":{"conversationId":"original-source-queue","messageId":"history-answer","revision":1,"text":"selected answer"}}}]`
	if _, err := st.AppendUserMessage(ctx, source.ID, op.SourceSessionID, op.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: "pending-message", ClientMessageID: "pending-client", ClientPayloadHash: "original-client-hash", Text: "A-Q28: B; Q29 unanswered", Origin: domain.MessageOriginHuman, DeliveryContentJSON: payload}, "pending-turn", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if promoting {
		if _, err := st.ReserveQueuedTurnForPromotion(ctx, source.ID, "pending-turn", at); err != nil {
			t.Fatal(err)
		}
		op, _, err = st.CreateOrchestratorReplacement(ctx, op)
		if err != nil {
			t.Fatal(err)
		}
	}
	op.Phase = domain.ReplacementStarting
	op.SpawnRequestID, op.SpawnRequestHash = "replacement/"+op.ID, op.RequestHash
	op = fixturestore.SeedReplacementTestFacts(t, st, op)
	old, _, err := st.GetSession(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	old.IsTerminated = true
	if err := st.UpdateSession(ctx, old); err != nil {
		t.Fatal(err)
	}
	if !activated {
		api, ok := any(st).(intakeReleaseStore)
		if !ok {
			t.Fatal("verified canonical intake transfer is unavailable")
		}
		return intakeTransferFixture{st: st, api: api, op: op, source: source, raw: raw, payload: payload}
	}
	writer := sampleRecord(string(op.ProjectID))
	writer.Kind, writer.Mode = domain.KindOrchestrator, domain.SessionModeChat
	writer.ClientRequestID, writer.ClientRequestHash = op.SpawnRequestID, op.SpawnRequestHash
	writer.ProvisionState = domain.SessionProvisionReady
	writer.Metadata.ControllerGeneration, writer.Metadata.ProviderConversationID, writer.Metadata.BrowserCapabilityVerifier = "successor-after", "native-successor", "resumed-verifier"
	writer, _, err = st.CreateClientRequestSession(ctx, writer)
	if err != nil {
		t.Fatal(err)
	}
	target, err := st.CreateProjectConversationAdoptingQueue(ctx, "project-target", op.ProjectID, writer.ID, domain.ConversationActivity{}, at)
	if err != nil {
		t.Fatal(err)
	}
	branch := domain.ConversationBranch{ID: "target-native-branch", ConversationID: target.ID, SessionID: writer.ID, ParentBranchID: target.ActiveBranchID, ProviderScopeID: "target-native-branch", ProviderConversationID: writer.Metadata.ProviderConversationID, Strategy: domain.ConversationBranchStrategyNative, ForkAfterSequence: target.LatestSequence, CreatedAt: at}
	if err := st.CommitChatSpawn(ctx, writer, branch); err != nil {
		t.Fatal(err)
	}
	writer, _, err = st.GetSession(ctx, writer.ID)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = domain.ReplacementNotifying
	op = fixturestore.SeedReplacementTestFacts(t, st, op)
	state := domain.ProjectDocumentState{Lease: &domain.ProjectDocumentLease{SessionID: writer.ID, Owner: writer.ControllerOwner(), Epoch: op.WriterActivation.AfterEpoch, ReplacementID: op.ID, ClaimVerifier: writer.Metadata.BrowserCapabilityVerifier}}
	if saved, err := st.SaveProjectDocumentState(ctx, op.ProjectID, state); err != nil || !saved {
		t.Fatal(saved, err)
	}
	worker := sampleRecord(string(op.ProjectID))
	worker, err = st.CreateSession(ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	if err := gen.New(raw).InsertConversationQueuedEditDelivery(ctx, gen.InsertConversationQueuedEditDeliveryParams{ConversationID: source.ID, ClientMessageID: "opaque-edit-receipt", RequestHash: "unchanged-edit-hash", CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	api, ok := any(st).(intakeReleaseStore)
	if !ok {
		t.Fatal("verified canonical intake transfer is unavailable")
	}
	return intakeTransferFixture{st, api, op, source, target, writer, worker, raw, payload}
}

func TestReplacementIntakeReleaseMovesSourceSessionQueueKeepsHistory(t *testing.T) {
	f := newIntakeTransferFixture(t)
	ctx := context.Background()
	history, err := f.st.TurnByID(ctx, "history-turn")
	if err != nil {
		t.Fatal(err)
	}
	worker, _, err := f.st.GetSession(ctx, f.worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	released, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now())
	if err != nil || !applied || released.IntakeTransfer == nil || released.IntakeTransfer.MovedMessages != 1 {
		t.Fatal(released, applied, err)
	}
	turn, err := f.st.TurnByID(ctx, "pending-turn")
	if err != nil || turn.ConversationID != f.target.ID || turn.HandledBySessionID != f.writer.ID || turn.State != domain.TurnStateQueued || turn.ProviderTurnID != "" || turn.StartedAt != nil {
		t.Fatal("turn identity or delivery changed", turn, err)
	}
	message, found, err := f.st.ConversationMessageByClientID(ctx, f.target.ID, "pending-client")
	if err != nil || !found || message.ID != "pending-message" || message.TurnID != turn.ID || message.DeliveryContentJSON != f.payload || message.ClientPayloadHash != "original-client-hash" {
		t.Fatal("canonical payload changed", message, found, err)
	}
	old, err := f.st.ConversationMessages(ctx, f.source.ID)
	if err != nil || len(old) != 2 || old[1].ID != "history-answer" {
		t.Fatal("completed history moved", old, err)
	}
	after, err := f.st.TurnByID(ctx, "history-turn")
	if err != nil || !reflect.DeepEqual(history, after) {
		t.Fatal("history facts changed", after, err)
	}
	nextWorker, _, err := f.st.GetSession(ctx, f.worker.ID)
	if err != nil || !reflect.DeepEqual(worker, nextWorker) {
		t.Fatal("worker changed", err)
	}
	edit, err := gen.New(f.raw).SelectConversationQueuedEditDelivery(ctx, gen.SelectConversationQueuedEditDeliveryParams{ConversationID: f.target.ID, ClientMessageID: "opaque-edit-receipt"})
	if err != nil || edit != "unchanged-edit-hash" {
		t.Fatal("edit receipt lost", edit, err)
	}
	replay, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now())
	if err != nil || !applied || replay.Revision != released.Revision {
		t.Fatal("release repeated after reply loss", applied, err)
	}
	batch, err := f.st.ClaimQueuedBatch(ctx, f.target.ID, true, 0, time.Now())
	if err != nil || len(batch) != 1 || batch[0].TurnID != turn.ID || batch[0].DeliveryContentJSON != f.payload {
		t.Fatal("original turn not drainable once", batch, err)
	}
}

func TestReplacementIntakeTransferRefusesUncertainProviderOrClientCollision(t *testing.T) {
	for _, fault := range []string{"provider", "provider input", "started marker", "promotion", "client collision", "generation", "capability", "lease epoch", "lease frozen"} {
		t.Run(fault, func(t *testing.T) {
			f := newIntakeTransferFixtureWithPromotion(t, fault == "promotion")
			ctx := context.Background()
			switch fault {
			case "provider", "provider input", "started marker":
				statement := `UPDATE conversation_turns SET provider_turn_id='uncertain-provider' WHERE id='pending-turn'`
				if fault == "started marker" {
					statement = `UPDATE conversation_turns SET started_at=CURRENT_TIMESTAMP WHERE id='pending-turn'`
				}
				if fault == "provider input" {
					statement = `UPDATE conversation_turns SET provider_input_text='possibly delivered' WHERE id='pending-turn'`
				}

				if _, err := f.raw.Exec(statement); err != nil {
					t.Fatal(err)
				}
			case "promotion":
				// Reservation existed before admission; the replacement must never replay it.
			case "client collision":
				if _, err := f.st.AppendUserMessage(ctx, f.target.ID, f.writer.ID, f.writer.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "distinct-message", ClientMessageID: "pending-client", ClientPayloadHash: "distinct-client-hash", Text: "distinct accepted input", Origin: domain.MessageOriginHuman}, "distinct-turn", time.Now()); err != nil {
					t.Fatal(err)
				}
			case "generation", "capability":
				row := f.writer
				if fault == "generation" {
					row.Metadata.ControllerGeneration = "rotated"
				} else {
					row.Metadata.BrowserCapabilityVerifier = "rotated"
				}
				if err := f.st.UpdateSession(ctx, row); err != nil {
					t.Fatal(err)
				}
			default:
				state, err := f.st.GetProjectDocumentState(ctx, f.op.ProjectID)
				if err != nil {
					t.Fatal(err)
				}
				if fault == "lease epoch" {
					state.Lease.Epoch++
				} else {
					state.Lease.Frozen = true
				}
				if saved, err := f.st.SaveProjectDocumentState(ctx, f.op.ProjectID, state); err != nil || !saved {
					t.Fatal(saved, err)
				}
			}
			_, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now())
			if applied {
				t.Fatal("unverified transfer accepted", fault)
			}
			if fault == "client collision" && !errors.Is(err, domain.ErrReplacementIntakeConflict) {
				t.Fatal(fault, err)
			}
			if (fault == "provider" || fault == "provider input" || fault == "started marker" || fault == "promotion") && !errors.Is(err, domain.ErrReplacementIntakeUncertain) {
				t.Fatal(fault, err)
			}
			current, _, readErr := f.st.GetOrchestratorReplacement(ctx, f.op.ID)
			if readErr != nil || current.IntakeReleasedAt != nil || current.IntakeTransfer != nil {
				t.Fatal("partial release", current, readErr)
			}
			message, found, err := f.st.ConversationMessageByClientID(ctx, f.source.ID, "pending-client")
			if err != nil || !found || message.DeliveryContentJSON != f.payload {
				t.Fatal("rejection lost input", found, err)
			}
		})
	}
}

func TestReplacementIntakeTransferRollbackIncludesFinalCAS(t *testing.T) {
	for _, fault := range []string{"cas", "ENOSPC"} {
		t.Run(fault, func(t *testing.T) {
			f := newIntakeTransferFixture(t)
			ctx := context.Background()
			trigger := `CREATE TRIGGER refuse_transfer_release BEFORE UPDATE ON orchestrator_replacements WHEN json_type(NEW.state_json,'$.intakeReleasedAt')='text' BEGIN SELECT RAISE(ABORT,'transfer ENOSPC'); END`
			if fault == "cas" {
				trigger = fmt.Sprintf(`CREATE TRIGGER race_transfer_release BEFORE UPDATE OF conversation_id ON conversation_messages WHEN OLD.id='pending-message' BEGIN UPDATE orchestrator_replacements SET revision=revision+1 WHERE id='%s'; END`, f.op.ID)
			}
			if _, err := f.raw.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if _, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now()); applied || err == nil {
				t.Fatal("failed release succeeded", applied, err)
			}
			current, _, err := f.st.GetOrchestratorReplacement(ctx, f.op.ID)
			if err != nil || current.Revision != f.op.Revision || current.IntakeReleasedAt != nil {
				t.Fatal("partial operation", current, err)
			}
			message, found, err := f.st.ConversationMessageByClientID(ctx, f.source.ID, "pending-client")
			if err != nil || !found || message.ID != "pending-message" {
				t.Fatal("stranded source input", found, err)
			}
			var receipts int
			if err := f.raw.QueryRow(`SELECT count(*) FROM orchestrator_replacement_intake_transfers WHERE operation_id=?`, f.op.ID).Scan(&receipts); err != nil || receipts != 0 {
				t.Fatal("partial receipt", receipts, err)
			}
		})
	}
}

func TestReplacementReleasedHumanProjectionRestoresInputWithoutNativeDelivery(t *testing.T) {
	for _, kind := range []string{"human", "user-authored automation", "automation"} {
		t.Run(kind, func(t *testing.T) {
			f := newIntakeTransferFixture(t)
			ctx := context.Background()
			writer := f.writer
			writer.Metadata.LatestUserPrompt, writer.Metadata.LatestAssistantUpdate = "verified native request", "verified native answer"
			writer.Metadata.LatestUserPromptAt = time.Now().Add(-time.Hour).UTC()
			writer.Metadata.ConversationCheckpointState = domain.ConversationCheckpointComplete
			writer.Metadata.ConversationCheckpointGeneration = writer.Metadata.ControllerGeneration
			writer.Metadata.ConversationCheckpointNativeID = writer.Metadata.ProviderConversationID
			writer.Metadata.ConversationCheckpointTurnID = "verified-native-turn"
			if err := f.st.UpdateSession(ctx, writer); err != nil {
				t.Fatal(err)
			}
			origin := domain.MessageOriginAutomation
			if kind == "human" {
				origin = domain.MessageOriginHuman
			}
			interaction := time.Now().Add(2 * time.Second).UTC()
			message := domain.ConversationMessage{ID: "recent-intake-message", Text: "edited human instruction", ClientMessageID: "recent-intake-client", Origin: origin, AuthoredByUser: kind == "user-authored automation", InteractionAt: interaction}
			if _, err := f.st.AppendUserMessage(ctx, f.target.ID, writer.ID, writer.Metadata.ControllerGeneration, message, "recent-intake-turn", time.Now()); err != nil {
				t.Fatal(err)
			}
			held, _, err := f.st.GetSession(ctx, writer.ID)
			if err != nil || held.Metadata.LatestUserPrompt != writer.Metadata.LatestUserPrompt || held.Metadata.ConversationCheckpointTurnID != "verified-native-turn" {
				t.Fatal("held input corrupted native checkpoint", held.Metadata, err)
			}
			if _, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now()); err != nil || !applied {
				t.Fatal(applied, err)
			}
			released, _, err := f.st.GetSession(ctx, writer.ID)
			if err != nil {
				t.Fatal(err)
			}
			expected := message.Text
			if kind == "automation" {
				expected = "A-Q28: B; Q29 unanswered"
			} else if !released.Metadata.LatestUserPromptAt.Equal(interaction) {
				t.Fatal("interaction time was lost", released.Metadata.LatestUserPromptAt, interaction)
			}
			if released.Metadata.LatestUserPrompt != expected || released.Metadata.LatestAssistantUpdate != "" || released.Metadata.ConversationCheckpointState != domain.ConversationCheckpointLegacy || released.Metadata.ConversationCheckpointTurnID != "" {
				t.Fatal("release did not restore unconfirmed human projection", released.Metadata)
			}
			queued, err := f.st.TurnByID(ctx, "recent-intake-turn")
			if err != nil || queued.ProviderTurnID != "" || queued.StartedAt != nil || queued.State != domain.TurnStateQueued {
				t.Fatal("projection fabricated native delivery", queued, err)
			}
			released.Metadata.ConversationCheckpointState = domain.ConversationCheckpointComplete
			released.Metadata.ConversationCheckpointTurnID = "later-verified-native-turn"
			if err := f.st.UpdateSession(ctx, released); err != nil {
				t.Fatal(err)
			}
			if _, _, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now()); err != nil {
				t.Fatal(err)
			}
			after, _, err := f.st.GetSession(ctx, writer.ID)
			if err != nil || after.Metadata.ConversationCheckpointTurnID != "later-verified-native-turn" {
				t.Fatal("reply loss rewrote later checkpoint", after.Metadata, err)
			}
		})
	}
}

func TestReplacementTransferredExcerptPermissionUsesCurrentOwnerAndExactPayload(t *testing.T) {
	for _, fault := range []string{"current", "generation", "capability", "lease epoch", "lease frozen", "source", "payload", "new hash"} {
		t.Run(fault, func(t *testing.T) {
			f := newIntakeTransferFixture(t)
			ctx := context.Background()
			if _, applied, err := f.api.ReleaseReplacementIntake(ctx, f.op.ID, f.op.Revision, time.Now()); err != nil || !applied {
				t.Fatal(applied, err)
			}
			message, _, err := f.st.ConversationMessageByClientID(ctx, f.target.ID, "pending-client")
			if err != nil {
				t.Fatal(err)
			}
			input := domain.QueuedTurn{TurnID: message.TurnID, Text: message.Text, ClientMessageID: message.ClientMessageID, Origin: message.Origin, DeliveryContentJSON: message.DeliveryContentJSON}
			hash, err := domain.ReplacementIntakePayloadHash(input)
			if err != nil {
				t.Fatal(err)
			}
			generation, source := f.writer.Metadata.ControllerGeneration, f.source.ID
			switch fault {
			case "generation":
				generation = "stale-generation"
			case "source":
				source = "worker-private-history"
			case "capability":
				row := f.writer
				row.Metadata.BrowserCapabilityVerifier = "rotated"
				if err := f.st.UpdateSession(ctx, row); err != nil {
					t.Fatal(err)
				}
			case "lease epoch", "lease frozen":
				state, err := f.st.GetProjectDocumentState(ctx, f.op.ProjectID)
				if err != nil {
					t.Fatal(err)
				}
				if fault == "lease epoch" {
					state.Lease.Epoch++
				} else {
					state.Lease.Frozen = true
				}
				if saved, err := f.st.SaveProjectDocumentState(ctx, f.op.ProjectID, state); err != nil || !saved {
					t.Fatal(saved, err)
				}
			case "payload", "new hash":
				if _, err := f.raw.Exec(`UPDATE conversation_messages SET text='changed after transfer' WHERE id='pending-message'`); err != nil {
					t.Fatal(err)
				}
				if fault == "new hash" {
					input.Text = "changed after transfer"
					hash, err = domain.ReplacementIntakePayloadHash(input)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			allowed, err := f.api.ReplacementExcerptSourceAllowed(ctx, f.writer.ID, generation, f.target.ID, input.TurnID, source, hash)
			if err != nil || allowed != (fault == "current") {
				t.Fatal("excerpt permission ignored owner/source/payload", fault, allowed, err)
			}
		})
	}
}
