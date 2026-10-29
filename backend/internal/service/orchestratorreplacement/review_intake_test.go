package orchestratorreplacement_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func replacementRawDB(t *testing.T, x *preparedExport) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(x.f.store.dataDir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func replacementIntakeRevision(t *testing.T, x *preparedExport) int64 {
	t.Helper()
	op, found, err := x.f.store.GetOrchestratorReplacement(context.Background(), x.operation.ID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	return op.IntakeRevision
}

func TestReplacementQueuedStateChangesInvalidatePreparedIntake(t *testing.T) {
	for _, scope := range []domain.ConversationScope{domain.ConversationScopeProject, domain.ConversationScopeSession} {
		for _, action := range []string{"cancel one", "interrupt queue"} {
			t.Run(string(scope)+"/"+action, func(t *testing.T) {
				x := preparingExportFixture(t)
				ctx := context.Background()
				conv := x.conversation
				if scope == domain.ConversationScopeSession {
					var err error
					conv, err = x.f.store.CreateConversation(ctx, "source-intake", scope, x.source.ProjectID, x.source.ID, time.Now())
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := x.f.store.AppendUserMessage(ctx, conv.ID, x.source.ID, x.source.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "cancel-intake", ClientMessageID: "cancel-intake", Text: "queued instruction", Origin: domain.MessageOriginHuman}, "cancel-intake-turn", time.Now()); err != nil {
					t.Fatal(err)
				}
				before := replacementIntakeRevision(t, x)
				var err error
				if action == "cancel one" {
					err = x.f.store.CancelQueuedTurnByID(ctx, conv.ID, "cancel-intake-turn", time.Now())
				} else {
					err = x.f.store.CancelQueuedTurns(ctx, conv.ID, time.Now().Add(time.Second), time.Now())
				}
				if err != nil {
					t.Fatal(err)
				}
				if after := replacementIntakeRevision(t, x); after != before+1 {
					t.Fatalf("queued state changed without invalidating section B: revision %d -> %d", before, after)
				}
			})
		}
	}
}

func TestReplacementIntakeMessageChangesExcludeOwnedControl(t *testing.T) {
	x := preparingExportFixture(t)
	ctx := context.Background()
	db := replacementRawDB(t, x)
	before := replacementIntakeRevision(t, x)
	appendMessage := func(id, client string, origin domain.MessageOrigin) {
		t.Helper()
		if _, err := x.f.store.AppendUserMessage(ctx, x.conversation.ID, x.source.ID, x.source.Metadata.ControllerGeneration, domain.ConversationMessage{ID: id, ClientMessageID: client, Text: id, Origin: origin}, id+"-turn", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	appendMessage("ordinary", "ordinary", domain.MessageOriginHuman)
	for i, query := range []string{"", `UPDATE conversation_messages SET text='edited' WHERE id='ordinary'`, `DELETE FROM conversation_messages WHERE id='ordinary'`} {
		if query != "" {
			if _, err := db.Exec(query); err != nil {
				t.Fatal(err)
			}
		}
		if got := replacementIntakeRevision(t, x); got != before+int64(i)+1 {
			t.Fatalf("message mutation %d not tracked: %d", i, got)
		}
	}
	before = replacementIntakeRevision(t, x)
	for _, query := range []string{`UPDATE conversation_messages SET text='control edit' WHERE id='preparation-message'`, `DELETE FROM conversation_messages WHERE id='preparation-message'`} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	appendMessage("reinsert-control", x.operation.Preparation.ControlClientID, domain.MessageOriginAutomation)
	if err := x.f.store.CancelQueuedTurnByID(ctx, x.conversation.ID, "reinsert-control-turn", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := replacementIntakeRevision(t, x); got != before {
		t.Fatalf("daemon control was treated as new user intake: %d -> %d", before, got)
	}
}

func TestReplacementSchemaDispatchFenceDistinguishesSourceWorkerAndControl(t *testing.T) {
	for _, which := range []string{"project input", "source input", "worker input", "control"} {
		t.Run(which, func(t *testing.T) {
			x := preparingExportFixture(t)
			ctx := context.Background()
			db := replacementRawDB(t, x)
			owner, conv, origin, client := x.source, x.conversation, domain.MessageOriginHuman, "fence-input"
			if which == "source input" || which == "worker input" {
				if which == "worker input" {
					owner = x.worker
				}
				var err error
				conv, err = x.f.store.CreateConversation(ctx, "scope-fence", domain.ConversationScopeSession, owner.ProjectID, owner.ID, time.Now())
				if err != nil {
					t.Fatal(err)
				}
			}
			if which == "control" {
				if _, err := db.Exec(`DELETE FROM conversation_messages WHERE id='preparation-message'`); err != nil {
					t.Fatal(err)
				}
				client, origin = x.operation.Preparation.ControlClientID, domain.MessageOriginAutomation
			}
			if _, err := x.f.store.AppendUserMessage(ctx, conv.ID, owner.ID, owner.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "fence-input", ClientMessageID: client, Text: "input", Origin: origin}, "fence-turn", time.Now()); err != nil {
				t.Fatal(err)
			}
			var held int
			if err := db.QueryRow(`SELECT COUNT(*) FROM orchestrator_replacement_held_turns WHERE turn_id='fence-turn'`).Scan(&held); err != nil {
				t.Fatal(err)
			}
			_, err := db.Exec(`UPDATE conversation_turns SET state='running' WHERE id='fence-turn'`)
			wantHeld := which != "worker input" && which != "control"
			if (held != 0) != wantHeld || (err != nil) != wantHeld {
				t.Fatalf("dispatch fence %s: held=%d err=%v", which, held, err)
			}
		})
	}
}

func TestReplacementPinnedCutoffExcludesNewProviderAdoption(t *testing.T) {
	x := preparedExportFixture(t)
	x.advanceUntil(t, func(op domain.OrchestratorReplacement) bool { return op.FrozenAt != nil })
	ctx := context.Background()
	before, err := x.f.store.Store.LoadTranscriptSourceSnapshot(ctx, x.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.f.store.AdoptProviderTurn(ctx, x.conversation.ID, x.source.ID, x.source.Metadata.ControllerGeneration, "late-adopted-turn", "late-provider-turn", time.Now()); err != nil {
		t.Fatal(err)
	}
	after, err := x.f.store.Store.LoadTranscriptSourceSnapshot(ctx, x.source.ID)
	if err != nil || before.Fingerprint != after.Fingerprint {
		t.Fatalf("post-freeze adoption entered the pinned cutoff: before=%s after=%s err=%v", before.Fingerprint, after.Fingerprint, err)
	}
}
