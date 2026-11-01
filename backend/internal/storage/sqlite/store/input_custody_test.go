package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestInputCustodyPreservesMixedInputsAcrossTransferAndReturn(t *testing.T) {
	ctx := t.Context()
	s, db, rec := settingsStartFixture(t, "")
	ids := []string{"queued", "held", "unknown"}
	for _, id := range ids {
		if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, "generation-1", domain.ConversationMessage{
			ID: "message-" + id, Text: "original " + id, ClientMessageID: "client-" + id,
			Origin: domain.MessageOriginHuman, DeliveryContentJSON: `[{"type":"text","text":"original attachment"}]`,
		}, id, testNow); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RecordProviderInput(ctx, "unknown", "original provider frame"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindTurnToProvider(ctx, "unknown", "provider-before-hold", testNow); err != nil {
		t.Fatal(err)
	}
	unknownBefore, err := s.TurnByID(ctx, "unknown")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := s.RegisterInputCustody(ctx, id, "settings:source", "register:"+id, testNow); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids[1:] {
		reason := "wake_blocked"
		if id == "unknown" {
			reason = "outcome_unknown"
		}
		if _, err := s.HoldConversationInput(ctx, domain.InputCustodyVersion{TurnID: id, Owner: "settings:source", Revision: 1},
			domain.InputHoldVersion{Key: "delivery", Owner: "settings:source"}, reason, "hold:"+id, testNow); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.ConversationInputCustody(ctx, "start-context")
	if err != nil || len(before) != 3 {
		t.Fatalf("mixed inventory: %+v %v", before, err)
	}
	for i, item := range before {
		if item.TurnID != ids[i] {
			t.Fatalf("equal-time input order changed: %+v", before)
		}
		for _, owner := range []string{"handoff:candidate-inbox", "project:unassigned-recovery", "settings:source"} {
			receipt, err := s.TransferInputCustody(ctx, item.InputCustodyVersion, owner, owner+":"+item.TurnID, testNow)
			if err != nil {
				t.Fatal(err)
			}
			item.Owner, item.Revision = owner, receipt.Revision
		}
	}
	after, err := s.ConversationInputCustody(ctx, "start-context")
	if err != nil {
		t.Fatal(err)
	}
	for i := range before {
		after[i].Revision = before[i].Revision
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("custody transfer changed provenance or holds:\nbefore=%+v\nafter=%+v", before, after)
	}
	unknownAfter, err := s.TurnByID(ctx, "unknown")
	if err != nil {
		t.Fatal(err)
	}
	unknownAfter.State = unknownBefore.State
	if !reflect.DeepEqual(unknownBefore, unknownAfter) {
		t.Fatalf("provider evidence changed: before=%+v after=%+v", unknownBefore, unknownAfter)
	}
	var text, content, frame string
	if err := db.QueryRow(`SELECT m.text,m.delivery_content_json,t.provider_input_text FROM conversation_messages m JOIN conversation_turns t ON t.id=m.turn_id WHERE t.id='unknown'`).Scan(&text, &content, &frame); err != nil {
		t.Fatal(err)
	}
	if text != "original unknown" || content != `[{"type":"text","text":"original attachment"}]` || frame != "original provider frame" {
		t.Fatalf("payload lost: %q %q %q", text, content, frame)
	}
	if err := s.InputCustodyStorageCompatibility(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestInputCustodyLayeredHoldsRequireExactOwnerAndVersion(t *testing.T) {
	ctx := t.Context()
	s, session, conversation := seededChatConversation(t)
	appendBranchPrompt(t, s, session, conversation, "generation-root", "held", "keep me")
	if _, err := s.RegisterInputCustody(ctx, "turn-held", "source", "register", testNow); err != nil {
		t.Fatal(err)
	}
	expected := domain.InputCustodyVersion{TurnID: "turn-held", Owner: "source", Revision: 1}
	for _, key := range []string{"settings", "handoff"} {
		receipt, err := s.HoldConversationInput(ctx, expected, domain.InputHoldVersion{Key: key, Owner: "source"}, "blocked", "hold:"+key, testNow)
		if err != nil {
			t.Fatal(err)
		}
		expected.Revision = receipt.Revision
	}
	if _, err := s.TransferInputCustody(ctx, expected, "target", "transfer", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseConversationInput(ctx, expected, domain.InputHoldVersion{Key: "settings", Owner: "source", Version: 1}, domain.TurnStateQueued, "stale-release", testNow); !errors.Is(err, domain.ErrInputCustodyConflict) {
		t.Fatalf("stale custodian released input: %v", err)
	}
	expected.Owner, expected.Revision = "target", expected.Revision+1
	for i, key := range []string{"settings", "handoff"} {
		if _, err := s.ReleaseConversationInput(ctx, expected, domain.InputHoldVersion{Key: key, Owner: "source", Version: 2}, domain.TurnStateQueued, "bad-version:"+key, testNow); !errors.Is(err, domain.ErrInputCustodyConflict) {
			t.Fatalf("wrong hold version accepted: %v", err)
		}
		receipt, err := s.ReleaseConversationInput(ctx, expected, domain.InputHoldVersion{Key: key, Owner: "source", Version: 1}, domain.TurnStateQueued, "release:"+key, testNow)
		if err != nil {
			t.Fatal(err)
		}
		expected.Revision = receipt.Revision
		turn, err := s.TurnByID(ctx, "turn-held")
		if err != nil || (i == 0 && turn.State != domain.TurnStateHeld) || (i == 1 && turn.State != domain.TurnStateQueued) {
			t.Fatalf("layer %s improperly released another hold: %+v %v", key, turn, err)
		}
	}
	if _, err := s.HoldConversationInput(ctx, expected, domain.InputHoldVersion{Key: "settings", Owner: "target", Version: 2}, "another_block", "rehold", testNow); err != nil {
		t.Fatalf("new hold generation: %v", err)
	}
}

func TestInputCustodyLegacySQLCannotMutateHeldButOrdinaryQueueWorks(t *testing.T) {
	ctx := t.Context()
	s, db, rec := settingsStartFixture(t, "")
	for _, id := range []string{"held", "ordinary"} {
		if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, "generation-1", domain.ConversationMessage{ID: "message-" + id, Text: id, Origin: domain.MessageOriginHuman}, id, testNow); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO conversation_activities(id,conversation_id,turn_id,sequence,kind,status,summary,created_at,updated_at) VALUES('held-activity','start-context','held',3,'command','completed','original evidence',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterInputCustody(ctx, "held", "settings", "register", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HoldConversationInput(ctx, domain.InputCustodyVersion{TurnID: "held", Owner: "settings", Revision: 1}, domain.InputHoldVersion{Key: "delivery", Owner: "settings"}, "legacy_unknown", "hold", testNow); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`UPDATE conversation_turns SET state='queued' WHERE id='held'`,
		`UPDATE conversation_turns SET state='running' WHERE id='held'`,
		`UPDATE conversation_turns SET state='failed' WHERE id='held'`,
		`UPDATE conversation_turns SET promotion_started_at=CURRENT_TIMESTAMP WHERE id='held'`,
		`UPDATE conversation_turns SET controller_generation='bypass' WHERE id='held'`,
		`UPDATE conversation_turns SET branch_id='bypass' WHERE id='held'`,
		`UPDATE conversation_messages SET text='lost' WHERE turn_id='held'`,
		`UPDATE conversation_messages SET turn_id=NULL WHERE turn_id='held'`,
		`DELETE FROM conversation_messages WHERE turn_id='held'`,
		`UPDATE conversation_activities SET turn_id='ordinary' WHERE id='held-activity'`,
		`DELETE FROM conversation_activities WHERE id='held-activity'`,
		`INSERT OR REPLACE INTO conversation_activities(id,conversation_id,turn_id,sequence,kind,status,summary,created_at,updated_at) VALUES('held-activity','start-context','ordinary',3,'command','completed','replaced',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`DELETE FROM conversation_turns WHERE id='held'`,
		`DELETE FROM conversation_input_holds WHERE turn_id='held'`,
		`UPDATE conversation_input_holds SET active=0 WHERE turn_id='held'`,
		`UPDATE conversation_input_custody SET owner='bypass' WHERE turn_id='held'`,
		`DELETE FROM conversation_input_actions WHERE action_key='hold'`,
		`INSERT OR REPLACE INTO conversation_turns SELECT * FROM conversation_turns WHERE id='held'`,
		`INSERT OR REPLACE INTO conversation_messages SELECT * FROM conversation_messages WHERE turn_id='held'`,
		`INSERT OR REPLACE INTO conversation_input_custody SELECT * FROM conversation_input_custody WHERE turn_id='held'`,
	} {
		if _, err := db.Exec(query); err == nil || !strings.Contains(err.Error(), "INPUT_") {
			t.Fatalf("unguarded legacy mutation %q: %v", query, err)
		}
	}
	queued, err := s.NextQueuedTurn(ctx, "start-context")
	if err != nil || queued.TurnID != "ordinary" {
		t.Fatalf("held head blocked ordinary work: %+v %v", queued, err)
	}
	if _, err := s.RetryPrompt(ctx, "start-context", "held"); err == nil {
		t.Fatal("held was offered as retryable")
	}
	if err := s.SettleOrphanedTurns(ctx, rec.ID, testNow); err != nil {
		t.Fatalf("held guard poisoned ordinary batch cleanup: %v", err)
	}
	for id, state := range map[string]domain.TurnState{"held": domain.TurnStateHeld, "ordinary": domain.TurnStateFailed} {
		turn, err := s.TurnByID(ctx, id)
		if err != nil || turn.State != state {
			t.Fatalf("legacy cleanup %s: %+v %v", id, turn, err)
		}
	}
}

func TestInputCustodyCASIsIdempotentAndRejectsConcurrentOwners(t *testing.T) {
	ctx := t.Context()
	s, rec, conversation := seededChatConversation(t)
	appendBranchPrompt(t, s, rec, conversation, "generation-root", "cas", "original")
	first, err := s.RegisterInputCustody(ctx, "turn-cas", "source", "register", testNow)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.TransferInputCustody(context.Background(), domain.InputCustodyVersion{TurnID: "turn-cas", Owner: "source", Revision: 1}, fmt.Sprintf("target-%d", i), fmt.Sprintf("transfer-%d", i), testNow)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	winners, conflicts := 0, 0
	for err := range results {
		if err == nil {
			winners++
		} else if errors.Is(err, domain.ErrInputCustodyConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("CAS winners=%d conflicts=%d", winners, conflicts)
	}
	repeated, err := s.RegisterInputCustody(ctx, "turn-cas", "source", "register", testNow)
	if err != nil || repeated != first {
		t.Fatalf("old receipt replayed instead of acknowledged: %+v %v", repeated, err)
	}
	if _, err := s.RegisterInputCustody(ctx, "turn-cas", "different", "register", testNow); !errors.Is(err, domain.ErrInputCustodyConflict) {
		t.Fatalf("action key reused with another request: %v", err)
	}
}

func TestInputCustodyExplicitDeleteRetainsActionHistory(t *testing.T) {
	ctx := t.Context()
	s, db, rec := settingsStartFixture(t, "")
	if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, "generation-1", domain.ConversationMessage{ID: "message", Text: "retained until explicit delete", Origin: domain.MessageOriginHuman}, "turn", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterInputCustody(ctx, "turn", "owner", "register", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HoldConversationInput(ctx, domain.InputCustodyVersion{TurnID: "turn", Owner: "owner", Revision: 1}, domain.InputHoldVersion{Key: "hold", Owner: "owner"}, "outcome_unknown", "hold", testNow); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.DeleteConversationInput(ctx, domain.InputCustodyVersion{TurnID: "turn", Owner: "owner", Revision: 2}, "delete", testNow); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"conversation_turns", "conversation_messages", "conversation_input_custody", "conversation_input_holds"} {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("explicit delete left %s: %d %v", table, count, err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_input_actions`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("delete erased receipts: %d %v", count, err)
	}
}

func TestInputCustodyActionsIgnoreUnrelatedForeignKeyOrphans(t *testing.T) {
	ctx := t.Context()
	s, db, rec := settingsStartFixture(t, "")
	if _, err := db.Exec(`CREATE TABLE legacy_orphans(id INTEGER PRIMARY KEY, project_id TEXT REFERENCES projects(id)); INSERT INTO legacy_orphans VALUES(1,'missing-project')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, "generation-1", domain.ConversationMessage{ID: "message", Text: "original", Origin: domain.MessageOriginHuman}, "turn", testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.InputCustodyStorageCompatibility(ctx); err != nil {
		t.Fatalf("unrelated orphan poisoned preflight: %v", err)
	}
	for range 2 {
		if _, err := s.RegisterInputCustody(ctx, "turn", "owner", "register", testNow); err != nil {
			t.Fatalf("unrelated orphan poisoned register/replay: %v", err)
		}
	}
	if _, err := s.HoldConversationInput(ctx, domain.InputCustodyVersion{TurnID: "turn", Owner: "owner", Revision: 1}, domain.InputHoldVersion{Key: "hold", Owner: "owner"}, "unknown", "hold", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransferInputCustody(ctx, domain.InputCustodyVersion{TurnID: "turn", Owner: "owner", Revision: 2}, "target", "transfer", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseConversationInput(ctx, domain.InputCustodyVersion{TurnID: "turn", Owner: "target", Revision: 3}, domain.InputHoldVersion{Key: "hold", Owner: "owner", Version: 1}, domain.TurnStateQueued, "release", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteConversationInput(ctx, domain.InputCustodyVersion{TurnID: "turn", Owner: "target", Revision: 4}, "delete", testNow); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM legacy_orphans`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("action changed legacy data: %d %v", count, err)
	}
}

func TestInputCustodyActionsEmitCDCRegardlessOfTriggerCreationOrder(t *testing.T) {
	ctx := t.Context()
	s, db, rec := settingsStartFixture(t, "")
	if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, "generation-1", domain.ConversationMessage{ID: "message", Text: "original", Origin: domain.MessageOriginHuman}, "turn", testNow); err != nil {
		t.Fatal(err)
	}
	// Changing an AFTER trigger's creation order must not lose delete's event.
	var apply string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='conversation_input_action_apply'`).Scan(&apply); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER conversation_input_action_apply`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(apply); err != nil {
		t.Fatal(err)
	}
	for i, action := range []func() error{
		func() error { _, err := s.RegisterInputCustody(ctx, "turn", "owner", "register", testNow); return err },
		func() error {
			_, err := s.TransferInputCustody(ctx, domain.InputCustodyVersion{TurnID: "turn", Owner: "owner", Revision: 1}, "target", "transfer", testNow)
			return err
		},
		func() error {
			_, err := s.DeleteConversationInput(ctx, domain.InputCustodyVersion{TurnID: "turn", Owner: "target", Revision: 2}, "delete", testNow)
			return err
		},
	} {
		for range 2 {
			if err := action(); err != nil {
				t.Fatal(err)
			}
		}
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE session_id=? AND json_extract(payload,'$.inputCustodyChanged')=1 AND json_extract(payload,'$.conversationId')='start-context'`, rec.ID).Scan(&count); err != nil || count != i+1 {
			t.Fatalf("CDC after action %d/replay: got %d, want %d: %v", i, count, i+1, err)
		}
	}
}

func TestInputCustodyMissingGuardFailsBeforeWriting(t *testing.T) {
	ctx := t.Context()
	s, db, rec := settingsStartFixture(t, "")
	if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, "generation-1", domain.ConversationMessage{ID: "message", Text: "original", Origin: domain.MessageOriginHuman}, "turn", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER conversation_input_turn_delete`); err != nil {
		t.Fatal(err)
	}
	if err := s.InputCustodyStorageCompatibility(ctx); !errors.Is(err, domain.ErrInputCustodyStorageIncompatible) || errors.Is(err, domain.ErrInputCustodyConflict) {
		t.Fatalf("missing guard must differ from CAS conflict: %v", err)
	}
	if _, err := s.RegisterInputCustody(ctx, "turn", "owner", "register", testNow); !errors.Is(err, domain.ErrInputCustodyStorageIncompatible) || errors.Is(err, domain.ErrInputCustodyConflict) {
		t.Fatalf("write was not fenced: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_input_actions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed gate wrote a receipt: %d %v", count, err)
	}
}

func TestInputCustodyChecksWriterForeignKeyEnforcement(t *testing.T) {
	for _, writerEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("writer=%t", writerEnabled), func(t *testing.T) {
			ctx := t.Context()
			s, reader, rec := settingsStartFixture(t, "")
			if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, "generation-1", domain.ConversationMessage{ID: "message", Text: "original", Origin: domain.MessageOriginHuman}, "turn", testNow); err != nil {
				t.Fatal(err)
			}
			var seq int
			var name, path string
			if err := reader.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
				t.Fatal(err)
			}
			writer, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = writer.Close() })
			writer.SetMaxOpenConns(1)
			reader.SetMaxOpenConns(1)
			if _, err := writer.Exec(fmt.Sprintf(`PRAGMA foreign_keys=%t`, writerEnabled)); err != nil {
				t.Fatal(err)
			}
			if _, err := reader.Exec(fmt.Sprintf(`PRAGMA foreign_keys=%t`, !writerEnabled)); err != nil {
				t.Fatal(err)
			}
			s = sqlitestore.NewStore(writer, reader)
			for _, check := range []func() error{
				func() error { return s.InputCustodyStorageCompatibility(ctx) },
				func() error { _, err := s.RegisterInputCustody(ctx, "turn", "owner", "register", testNow); return err },
			} {
				err := check()
				if writerEnabled && err != nil {
					t.Fatalf("disabled reader poisoned enabled writer: %v", err)
				}
				if !writerEnabled && (!errors.Is(err, domain.ErrInputCustodyStorageIncompatible) || errors.Is(err, domain.ErrInputCustodyConflict)) {
					t.Fatalf("disabled writer qualified through enabled reader: %v", err)
				}
			}
		})
	}
}

func TestInputCustodyActionsCheckOnlyTheirTargetTurn(t *testing.T) {
	ctx := t.Context()
	s, db, rec := settingsStartFixture(t, "")
	for _, id := range []string{"valid", "corrupt"} {
		if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, "generation-1", domain.ConversationMessage{ID: "message-" + id, Text: id, Origin: domain.MessageOriginHuman}, id, testNow); err != nil {
			t.Fatal(err)
		}
	}
	var guard string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='conversation_input_turn_update'`).Scan(&guard); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TRIGGER conversation_input_turn_update; UPDATE conversation_turns SET state='held' WHERE id='corrupt'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(guard); err != nil {
		t.Fatal(err)
	}
	if err := s.InputCustodyStorageCompatibility(ctx); !errors.Is(err, domain.ErrInputCustodyStorageIncompatible) {
		t.Fatalf("preflight missed global held/hold inconsistency: %v", err)
	}
	if _, err := s.RegisterInputCustody(ctx, "valid", "owner", "register-valid", testNow); err != nil {
		t.Fatalf("unrelated corrupt turn blocked healthy target: %v", err)
	}
	if _, err := s.RegisterInputCustody(ctx, "corrupt", "owner", "register-corrupt", testNow); !errors.Is(err, domain.ErrInputCustodyStorageIncompatible) || errors.Is(err, domain.ErrInputCustodyConflict) {
		t.Fatalf("target corruption must be rejected independently of CAS: %v", err)
	}
}
