package store_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

type inputReplacementCase struct {
	name, table, column, original, ordinary string
	value                                   any
}

var inputReplacementCases = []inputReplacementCase{
	{"message-client", "conversation_messages", "client_message_id", "message-protected", "message-ordinary", "stable-key"},
	{"message-sequence", "conversation_messages", "sequence", "message-protected", "message-ordinary", 1},
	{"message-provider", "conversation_messages", "provider_item_id", "message-protected", "message-ordinary", "provider-key"},
	{"activity-request", "conversation_activities", "request_id", "activity-protected", "activity-ordinary", "request-key"},
	{"activity-sequence", "conversation_activities", "sequence", "activity-protected", "activity-ordinary", 3},
	{"activity-provider", "conversation_activities", "provider_item_id", "activity-protected", "activity-ordinary", "activity-key"},
}

func inputReplacementFixture(t *testing.T, mode string, emptyKeys bool) (*sqlite.Store, *sql.DB) {
	t.Helper()
	ctx := t.Context()
	s, db, rec := settingsStartFixture(t, "")
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA recursive_triggers=OFF`); err != nil {
		t.Fatal(err)
	}
	var recursive int
	if err := db.QueryRow(`PRAGMA recursive_triggers`).Scan(&recursive); err != nil || recursive != 0 {
		t.Fatalf("replacement must be safe without recursive delete triggers: %d %v", recursive, err)
	}
	for _, id := range []string{"protected", "ordinary"} {
		client := ""
		if !emptyKeys && id == "protected" {
			client = "stable-key"
		}
		if _, err := s.AppendUserMessage(ctx, "start-context", rec.ID, "generation-1", domain.ConversationMessage{ID: "message-" + id, Text: "original " + id, ClientMessageID: client, Origin: domain.MessageOriginHuman}, id, testNow); err != nil {
			t.Fatal(err)
		}
	}
	provider, request, activity := "provider-key", "request-key", "activity-key"
	if emptyKeys {
		provider, request, activity = "", "", ""
	}
	if _, err := db.Exec(`UPDATE conversation_messages SET provider_item_id=? WHERE id='message-protected'`, provider); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conversation_activities(id,conversation_id,turn_id,sequence,kind,status,summary,request_id,provider_item_id,created_at,updated_at)
 VALUES('activity-protected','start-context','protected',3,'command','completed','original evidence',?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
 ('activity-ordinary','start-context','ordinary',4,'command','completed','ordinary evidence','','',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, request, activity); err != nil {
		t.Fatal(err)
	}
	if mode != "ordinary" {
		if _, err := s.RegisterInputCustody(ctx, "protected", "owner", "register", testNow); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "held" {
		if _, err := s.HoldConversationInput(ctx, domain.InputCustodyVersion{TurnID: "protected", Owner: "owner", Revision: 1}, domain.InputHoldVersion{Key: "delivery", Owner: "owner"}, "outcome_unknown", "hold", testNow); err != nil {
			t.Fatal(err)
		}
	}
	return s, db
}

func inputReplacementSQL(c inputReplacementCase, verb string) (string, []any) {
	if verb == "UPDATE" {
		content := "text"
		if c.table == "conversation_activities" {
			content = "summary"
		}
		return fmt.Sprintf("UPDATE OR REPLACE %s SET %s=?,%s='replacement' WHERE id=?", c.table, c.column, content), []any{c.value, c.ordinary}
	}
	var sequence any = 99
	key, provider := any(""), any("")
	switch c.column {
	case "sequence":
		sequence = c.value
	case "provider_item_id":
		provider = c.value
	default:
		key = c.value
	}
	if c.table == "conversation_messages" {
		return `INSERT OR REPLACE INTO conversation_messages(id,conversation_id,turn_id,sequence,role,origin,text,client_message_id,provider_item_id,created_at,updated_at)
 VALUES('incoming','start-context','ordinary',?,'user','human','replacement',?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, []any{sequence, key, provider}
	}
	return `INSERT OR REPLACE INTO conversation_activities(id,conversation_id,turn_id,sequence,kind,status,summary,request_id,provider_item_id,created_at,updated_at)
 VALUES('incoming','start-context','ordinary',?,'command','completed','replacement',?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, []any{sequence, key, provider}
}

func inputPayloadRow(t *testing.T, db *sql.DB, table, id string) string {
	t.Helper()
	rows, err := db.Query("SELECT * FROM "+table+" WHERE id=?", id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("missing original payload %s/%s: %v", table, id, rows.Err())
	}
	values, targets := make([]any, len(columns)), make([]any, len(columns))
	for i := range values {
		targets[i] = &values[i]
	}
	if err := rows.Scan(targets...); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestInputCustodyReplacementUniqueKeys(t *testing.T) {
	for _, mode := range []string{"held", "registered", "ordinary"} {
		for _, verb := range []string{"INSERT", "UPDATE"} {
			for _, c := range inputReplacementCases {
				t.Run(mode+"/"+verb+"/"+c.name, func(t *testing.T) {
					s, db := inputReplacementFixture(t, mode, false)
					before := inputPayloadRow(t, db, c.table, c.original)
					query, args := inputReplacementSQL(c, verb)
					_, err := db.Exec(query, args...)
					if mode == "ordinary" {
						if err != nil {
							t.Fatalf("v1 ordinary replacement blocked: %v", err)
						}
						var count int
						if err := db.QueryRow("SELECT COUNT(*) FROM "+c.table+" WHERE id=?", c.original).Scan(&count); err != nil || count != 0 {
							t.Fatalf("ordinary SQLite replacement changed behavior: %d %v", count, err)
						}
					} else {
						if err == nil || !strings.Contains(err.Error(), "INPUT_") {
							t.Fatalf("protected unique-key replacement accepted: %v", err)
						}
						if after := inputPayloadRow(t, db, c.table, c.original); after != before {
							t.Fatalf("replacement changed original payload: %s -> %s", before, after)
						}
					}
					if err := s.InputCustodyStorageCompatibility(t.Context()); err != nil {
						t.Fatalf("replacement attempt damaged shared facts: %v", err)
					}
				})
			}
		}
	}
}

func TestInputCustodyGateRejectsLostPayloadAfterReplacement(t *testing.T) {
	for _, verb := range []string{"INSERT", "UPDATE"} {
		for _, c := range inputReplacementCases {
			t.Run(verb+"/"+c.name, func(t *testing.T) {
				s, db := inputReplacementFixture(t, "held", false)
				kind := "message"
				if c.table == "conversation_activities" {
					kind = "activity"
				}
				name := "conversation_input_" + kind + "_" + strings.ToLower(verb)
				var guard string
				if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name=?`, name).Scan(&guard); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("DROP TRIGGER " + name); err != nil {
					t.Fatal(err)
				}
				query, args := inputReplacementSQL(c, verb)
				if _, err := db.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(guard); err != nil {
					t.Fatal(err)
				}
				var rows, active, held int
				if err := db.QueryRow("SELECT COUNT(*) FROM "+c.table+" WHERE id=?", c.original).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_input_holds WHERE turn_id='protected' AND active=1`).Scan(&active); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_turns WHERE id='protected' AND state='held'`).Scan(&held); err != nil {
					t.Fatal(err)
				}
				if rows != 0 || active != 1 || held != 1 {
					t.Fatalf("invalid loss reproduction: originalRows=%d activeHold=%d stateHeld=%d", rows, active, held)
				}
				for _, err := range []error{
					s.InputCustodyStorageCompatibility(t.Context()),
					func() error {
						_, err := s.TransferInputCustody(t.Context(), domain.InputCustodyVersion{TurnID: "protected", Owner: "owner", Revision: 2}, "target", "transfer", testNow)
						return err
					}(),
				} {
					if !errors.Is(err, domain.ErrInputCustodyStorageIncompatible) {
						t.Fatalf("gate qualified lost payload: %v", err)
					}
				}
			})
		}
	}
}

func TestInputCustodyReplacementAllowsEmptyKeysAndRegisteredProviderWrites(t *testing.T) {
	for _, mode := range []string{"held", "registered"} {
		t.Run(mode, func(t *testing.T) {
			s, db := inputReplacementFixture(t, mode, true)
			before := inputPayloadRow(t, db, "conversation_messages", "message-protected")
			for _, query := range []string{
				`INSERT OR REPLACE INTO conversation_messages(id,conversation_id,turn_id,sequence,role,origin,text,created_at,updated_at) VALUES('empty-key-message','start-context','ordinary',90,'assistant','provider','normal output',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
				`INSERT OR REPLACE INTO conversation_activities(id,conversation_id,turn_id,sequence,kind,status,summary,created_at,updated_at) VALUES('empty-key-activity','start-context','ordinary',91,'command','running','normal activity',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
				`UPDATE OR REPLACE conversation_messages SET client_message_id='',provider_item_id='',text='normal delta' WHERE id='empty-key-message'`,
				`UPDATE OR REPLACE conversation_activities SET request_id='',provider_item_id='',summary='normal update' WHERE id='empty-key-activity'`,
			} {
				if _, err := db.Exec(query); err != nil {
					t.Fatalf("empty keys falsely collided with protected payload: %v", err)
				}
			}
			if after := inputPayloadRow(t, db, "conversation_messages", "message-protected"); before != after {
				t.Fatal("ordinary empty-key writes changed protected message")
			}
			if mode == "registered" {
				for _, query := range []string{
					`UPDATE OR REPLACE conversation_messages SET streaming=1,text='legitimate delta' WHERE id='message-protected'`,
					`UPDATE OR REPLACE conversation_activities SET summary='legitimate progress' WHERE id='activity-protected'`,
					`INSERT INTO conversation_messages(id,conversation_id,turn_id,sequence,role,origin,text,client_message_id,created_at,updated_at) VALUES('later-message','start-context','protected',92,'assistant','provider','later payload','later-key',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
				} {
					if _, err := db.Exec(query); err != nil {
						t.Fatalf("registered non-held provider write blocked: %v", err)
					}
				}
				if err := s.InputCustodyStorageCompatibility(t.Context()); err != nil {
					t.Fatalf("registered provider writes failed preflight: %v", err)
				}
				if _, err := s.HoldConversationInput(t.Context(), domain.InputCustodyVersion{TurnID: "protected", Owner: "owner", Revision: 1}, domain.InputHoldVersion{Key: "delivery", Owner: "owner"}, "unknown", "hold", testNow); err != nil {
					t.Fatal(err)
				}
				// Hold must also retain rows appended after register's original snapshot.
				var guard string
				if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='conversation_input_message_insert'`).Scan(&guard); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`DROP TRIGGER conversation_input_message_insert; INSERT OR REPLACE INTO conversation_messages(id,conversation_id,turn_id,sequence,role,origin,text,client_message_id,created_at,updated_at) VALUES('intruder','start-context','ordinary',99,'user','human','lost later payload','later-key',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(guard); err != nil {
					t.Fatal(err)
				}
				if err := s.InputCustodyStorageCompatibility(t.Context()); !errors.Is(err, domain.ErrInputCustodyStorageIncompatible) {
					t.Fatalf("hold did not capture later payload identities: %v", err)
				}
			} else if err := s.InputCustodyStorageCompatibility(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInputCustodyChecksPayloadConversation(t *testing.T) {
	for _, c := range []struct{ table, id string }{
		{"conversation_messages", "message-protected"},
		{"conversation_activities", "activity-protected"},
		{"conversation_turns", "protected"},
	} {
		for _, route := range []string{"api", "sql"} {
			t.Run(c.table+"/"+route, func(t *testing.T) {
				s, db := inputReplacementFixture(t, "registered", false)
				if _, err := db.Exec(`INSERT INTO projects(id,path,registered_at) VALUES('other-project','other-project',CURRENT_TIMESTAMP);
INSERT INTO conversations(id,scope,project_id,created_at,updated_at) VALUES('other-context','project','other-project',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
					t.Fatal(err)
				}
				const snapshot = `SELECT json_array(owner,revision,last_action_key,original_conversation_id,message_ids_json,activity_ids_json) FROM conversation_input_custody WHERE turn_id='protected'`
				var before, after string
				if err := db.QueryRow(snapshot).Scan(&before); err != nil {
					t.Fatal(err)
				}
				if c.table == "conversation_turns" {
					// A whole transfer may change the current conversation while
					// register retains the original conversation as provenance.
					for _, table := range []string{"conversation_messages", "conversation_activities"} {
						if _, err := db.Exec("UPDATE " + table + " SET conversation_id='other-context' WHERE turn_id='protected'"); err != nil {
							t.Fatal(err)
						}
					}
				}
				if _, err := db.Exec("UPDATE "+c.table+" SET conversation_id='other-context' WHERE id=?", c.id); err != nil {
					t.Fatalf("could not reproduce registered conversation change with guards intact: %v", err)
				}
				var violations int
				if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
					t.Fatalf("fixture must retain valid FK references: %d %v", violations, err)
				}
				if c.table != "conversation_turns" {
					var visible int
					if err := db.QueryRow("SELECT COUNT(*) FROM "+c.table+" p JOIN conversation_turns t ON t.id=p.turn_id AND t.conversation_id=p.conversation_id WHERE p.id=?", c.id).Scan(&visible); err != nil || visible != 0 {
						t.Fatalf("fixture must hide the moved payload from its canonical turn: %d %v", visible, err)
					}
				}
				whole := c.table == "conversation_turns"
				checkGate := func() {
					t.Helper()
					err := s.InputCustodyStorageCompatibility(t.Context())
					if whole && err != nil {
						t.Errorf("preflight rejected whole conversation transfer: %v", err)
					} else if !whole && !errors.Is(err, domain.ErrInputCustodyStorageIncompatible) {
						t.Errorf("preflight accepted unilateral conversation change: %v", err)
					}
				}
				checkGate()
				var err error
				if route == "api" {
					_, err = s.HoldConversationInput(t.Context(), domain.InputCustodyVersion{TurnID: "protected", Owner: "owner", Revision: 1}, domain.InputHoldVersion{Key: "delivery", Owner: "owner"}, "unknown", "hold", testNow)
				} else {
					_, err = db.Exec(`INSERT INTO conversation_input_actions(action_key,turn_id,kind,expected_owner,expected_revision,hold_key,hold_owner,hold_version,reason,created_at)
VALUES('hold','protected','hold','owner',1,'delivery','owner',0,'unknown',CURRENT_TIMESTAMP)`)
				}
				if whole {
					if err != nil {
						t.Errorf("hold rejected whole conversation transfer: %v", err)
					}
				} else if route == "api" {
					if !errors.Is(err, domain.ErrInputCustodyStorageIncompatible) {
						t.Errorf("hold accepted unilateral conversation change: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "INPUT_CUSTODY_PAYLOAD_CHANGED") {
					t.Errorf("SQL hold accepted unilateral conversation change: %v", err)
				}
				expected := before
				wantActions, wantHolds, wantHeld := 1, 0, 0
				if whole {
					expected = `["owner",2,"hold","start-context","[\"message-protected\"]","[\"activity-protected\"]"]`
					wantActions, wantHolds, wantHeld = 2, 1, 1
				}
				if err := db.QueryRow(snapshot).Scan(&after); err != nil || after != expected {
					t.Errorf("failed hold changed custody baseline: %s -> %s: %v", before, after, err)
				}
				var actions, holds, held int
				if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM conversation_input_actions),(SELECT COUNT(*) FROM conversation_input_holds),(SELECT COUNT(*) FROM conversation_turns WHERE id='protected' AND state='held')`).Scan(&actions, &holds, &held); err != nil || actions != wantActions || holds != wantHolds || held != wantHeld {
					t.Errorf("unexpected hold facts: actions=%d holds=%d held=%d: %v", actions, holds, held, err)
				}
				checkGate()
			})
		}
	}
}
