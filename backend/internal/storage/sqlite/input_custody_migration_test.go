package sqlite

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"
)

func seedSharedInputMigration(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO projects(id,path,registered_at) VALUES('project','isolated-project',CURRENT_TIMESTAMP)`,
		`INSERT INTO sessions(id,project_id,num,activity_last_at,created_at,updated_at) VALUES('worker','project',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`INSERT INTO conversations(id,scope,project_id,session_id,current_session_id,created_at,updated_at) VALUES('conversation','session','project','worker','worker',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`INSERT INTO conversation_turns(rowid,id,conversation_id,handled_by_session_id,state,requested_at,intake_sequence) VALUES(11,'base','conversation','worker','queued','2026-11-01T00:00:00Z',1)`,
		`INSERT INTO conversation_turns(rowid,id,conversation_id,handled_by_session_id,state,requested_at,retry_of_turn_id,intake_sequence) VALUES(22,'retry','conversation','worker','failed','2026-11-01T00:00:00Z','base',2)`,
		`INSERT INTO conversation_turns(rowid,id,conversation_id,handled_by_session_id,state,requested_at,promoted_to_turn_id,intake_sequence) VALUES(33,'promoted','conversation','worker','completed','2026-11-01T00:00:00Z','base',3)`,
		`INSERT INTO conversation_messages(id,conversation_id,turn_id,sequence,role,origin,text,delivery_content_json,created_at,updated_at) VALUES('message','conversation','base',1,'user','human','original bytes','[{"type":"text","text":"attachment"}]',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`INSERT INTO conversation_branches(id,conversation_id,session_id,fork_after_turn_id,replaced_turn_id,replacement_turn_id,created_at) VALUES('branch','conversation','worker','base','retry','promoted',CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed %s: %v", statement, err)
		}
	}
}

func sharedInputObjects(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT type||':'||name,sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY type,name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	result := map[string]string{}
	for rows.Next() {
		var key, definition string
		if err := rows.Scan(&key, &definition); err != nil {
			t.Fatal(err)
		}
		result[key] = definition
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func assertSharedInputRows(t *testing.T, db *sql.DB) {
	t.Helper()
	var order, linked string
	if err := db.QueryRow(`SELECT group_concat(id||':'||rowid,',') FROM (SELECT id,rowid FROM conversation_turns ORDER BY requested_at,rowid)`).Scan(&order); err != nil || order != "base:11,retry:22,promoted:33" {
		t.Fatalf("queue rowid order = %q %v", order, err)
	}
	if err := db.QueryRow(`SELECT m.turn_id||':'||m.text||':'||b.fork_after_turn_id||':'||b.replaced_turn_id||':'||b.replacement_turn_id FROM conversation_messages m JOIN conversation_branches b ON b.id='branch' WHERE m.id='message'`).Scan(&linked); err != nil || linked != "base:original bytes:base:retry:promoted" {
		t.Fatalf("FK links or input bytes = %q %v", linked, err)
	}
	var count, foreignKeys int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("FK violations = %d %v", count, err)
	}
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("FK enforcement not restored: %d %v", foreignKeys, err)
	}
}

func TestSharedInputMigrationPreservesInventoryAndUpDownUp(t *testing.T) {
	db := openMigratedDatabaseCopy(t, sharedInputCustodyVersion-1)
	seedSharedInputMigration(t, db)
	before := sharedInputObjects(t, db)
	upTo(t, db, sharedInputCustodyVersion)
	after := sharedInputObjects(t, db)
	for key, definition := range before {
		got := after[key]
		switch key {
		case "table:conversation_turns":
			got = strings.ReplaceAll(got, "'cancelled', 'held'", "'cancelled'")
		case "view:orchestrator_replacement_held_turns":
			got = strings.Split(got, "\nUNION\nSELECT 'shared:'")[0]
		}
		if got != definition {
			t.Errorf("changed existing object %s:\nwant %s\ngot %s", key, definition, got)
		}
	}
	assertSharedInputRows(t, db)
	downTo(t, db, sharedInputCustodyVersion-1)
	if reverted := sharedInputObjects(t, db); !reflect.DeepEqual(before, reverted) {
		for key, definition := range before {
			if reverted[key] != definition {
				t.Errorf("Down changed %s", key)
			}
		}
		if len(before) != len(reverted) {
			t.Errorf("Down object count %d, want %d", len(reverted), len(before))
		}
	}
	assertSharedInputRows(t, db)
	upTo(t, db, sharedInputCustodyVersion)
	if reapplied := sharedInputObjects(t, db); !reflect.DeepEqual(after, reapplied) {
		t.Fatal("up/down/up did not restore complete guarded schema")
	}
	assertSharedInputRows(t, db)
}

func TestSharedInputMigrationPreservesExistingForeignKeyViolations(t *testing.T) {
	db := openMigratedDatabaseCopy(t, sharedInputCustodyVersion-1)
	seedSharedInputMigration(t, db)
	for _, statement := range []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE legacy_orphans(id INTEGER PRIMARY KEY, project_id TEXT REFERENCES projects(id))`,
		`INSERT INTO legacy_orphans VALUES(1,'missing-project')`,
		`INSERT INTO conversation_messages(id,conversation_id,turn_id,sequence,role,origin,text,created_at,updated_at) VALUES('old-orphan','conversation','missing-turn',2,'user','human','legacy input',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`PRAGMA foreign_keys=ON`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	before := sharedInputForeignKeyViolations(t, db)
	if len(before) != 2 {
		t.Fatalf("expected unrelated and existing turn FK orphans: %v", before)
	}
	for _, version := range []int64{sharedInputCustodyVersion, sharedInputCustodyVersion - 1, sharedInputCustodyVersion} {
		if version == sharedInputCustodyVersion {
			upTo(t, db, version)
		} else {
			downTo(t, db, version)
		}
		if after := sharedInputForeignKeyViolations(t, db); !reflect.DeepEqual(before, after) {
			t.Fatalf("migration changed legacy FK violations: %v -> %v", before, after)
		}
		var text string
		if err := db.QueryRow(`SELECT text FROM conversation_messages WHERE id='old-orphan'`).Scan(&text); err != nil || text != "legacy input" {
			t.Fatalf("lost legacy input: %q %v", text, err)
		}
	}
}

func sharedInputForeignKeyViolations(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT "table"||':'||rowid||':'||parent||':'||fkid FROM pragma_foreign_key_check ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var result []string
	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			t.Fatal(err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSharedInputMigrationRejectsNewForeignKeyViolations(t *testing.T) {
	const filename = "migrations/20261101152248341_shared_input_custody.sql"
	source, err := migrationsFS.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{"up", "down"} {
		t.Run(direction, func(t *testing.T) {
			version := sharedInputCustodyVersion - 1
			if direction == "down" {
				version = sharedInputCustodyVersion
			}
			db := openMigratedDatabaseCopy(t, version)
			seedSharedInputMigration(t, db)
			before := sharedInputObjects(t, db)
			var ledgerBefore int64
			if err := db.QueryRow(`SELECT MAX(id) FROM goose_db_version`).Scan(&ledgerBefore); err != nil {
				t.Fatal(err)
			}
			parts := strings.SplitN(string(source), "-- +goose Down", 2)
			part := 0
			if direction == "down" {
				part = 1
			}
			// Simulate a rebuild that loses a turn's parent after taking its FK baseline.
			parts[part] = strings.Replace(parts[part], "CREATE TEMP TABLE shared_input_fk_check", `UPDATE conversation_turns SET conversation_id='missing-parent' WHERE id='base';
CREATE TEMP TABLE shared_input_fk_check`, 1)
			fs := fstest.MapFS{filename: {Data: []byte(strings.Join(parts, "-- +goose Down"))}}
			err := func() error {
				gooseMu.Lock()
				defer gooseMu.Unlock()
				goose.SetBaseFS(fs)
				defer goose.SetBaseFS(migrationsFS)
				return withInputRebuildForeignKeys(db, func() error {
					if direction == "down" {
						return goose.DownTo(db, "migrations", sharedInputCustodyVersion-1)
					}
					return goose.UpTo(db, "migrations", sharedInputCustodyVersion)
				})
			}()
			if err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
				t.Fatalf("new FK violation did not reject rebuild: %v", err)
			}
			if after := sharedInputObjects(t, db); !reflect.DeepEqual(before, after) {
				t.Fatal("FK failure did not roll back complete schema")
			}
			assertSharedInputRows(t, db)
			var ledgerAfter int64
			if err := db.QueryRow(`SELECT MAX(id) FROM goose_db_version`).Scan(&ledgerAfter); err != nil || ledgerAfter != ledgerBefore {
				t.Fatalf("FK failure committed migration ledger: %d -> %d: %v", ledgerBefore, ledgerAfter, err)
			}
		})
	}
}

func TestSharedInputMigrationFailureRollsBackCompleteSchemaAndLedger(t *testing.T) {
	for _, collision := range []string{"ledger", "conversation_input_holds"} {
		t.Run(collision, func(t *testing.T) {
			db := openMigratedDatabaseCopy(t, sharedInputCustodyVersion-1)
			seedSharedInputMigration(t, db)
			fault := "CREATE TABLE " + collision + "(collision TEXT)"
			undo := "DROP TABLE " + collision
			if collision == "ledger" {
				fault = `CREATE TRIGGER s0_ledger_fault BEFORE INSERT ON goose_db_version WHEN NEW.version_id=20261101152248341 BEGIN SELECT RAISE(ABORT,'S0_LEDGER_FAULT'); END`
				undo = "DROP TRIGGER s0_ledger_fault"
			}
			if _, err := db.Exec(fault); err != nil {
				t.Fatal(err)
			}
			before := sharedInputObjects(t, db)
			if err := migrate(db); err == nil {
				t.Fatal("injected rebuild collision succeeded")
			}
			if after := sharedInputObjects(t, db); !reflect.DeepEqual(before, after) {
				t.Fatal("failed rebuild exposed a changed or unguarded schema")
			}
			assertSharedInputRows(t, db)
			var applied int
			if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id=?`, sharedInputCustodyVersion).Scan(&applied); err != nil || applied != 0 {
				t.Fatalf("failed schema committed its ledger: %d %v", applied, err)
			}
			if _, err := db.Exec(undo); err != nil {
				t.Fatal(err)
			}
			if err := migrate(db); err != nil {
				t.Fatalf("recovery after rollback: %v", err)
			}
			assertSharedInputRows(t, db)
		})
	}
}

func TestSharedInputMigrationRefusesUnqualifiedDirectRebuild(t *testing.T) {
	db := openMigratedDatabaseCopy(t, sharedInputCustodyVersion-1)
	seedSharedInputMigration(t, db)
	before := sharedInputObjects(t, db)
	gooseMu.Lock()
	err := goose.UpTo(db, "migrations", sharedInputCustodyVersion)
	gooseMu.Unlock()
	if err == nil {
		t.Fatal("rebuild with live FK cascades succeeded")
	}
	if after := sharedInputObjects(t, db); !reflect.DeepEqual(before, after) {
		t.Fatal("direct rebuild changed schema before rejecting")
	}
	assertSharedInputRows(t, db)
}

func TestSharedInputDownRefusesCustodyOrHistoricalActions(t *testing.T) {
	db := openMigratedDatabaseCopy(t, sharedInputCustodyVersion)
	seedSharedInputMigration(t, db)
	if _, err := db.Exec(`INSERT INTO conversation_input_actions(action_key,turn_id,kind,expected_owner,expected_revision,target_owner,created_at) VALUES('register','base','register','',0,'owner',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	before := sharedInputObjects(t, db)
	gooseMu.Lock()
	err := migrateDownTo(db, sharedInputCustodyVersion-1)
	gooseMu.Unlock()
	if err == nil {
		t.Fatal("Down discarded custody")
	}
	if after := sharedInputObjects(t, db); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected Down removed a guard")
	}
	assertSharedInputRows(t, db)
	if _, err := db.Exec(`INSERT INTO conversation_input_actions(action_key,turn_id,kind,expected_owner,expected_revision,created_at) VALUES('delete','base','delete','owner',1,CURRENT_TIMESTAMP)`); err == nil {
		// The FK-protected branch/retry links intentionally prevent this delete.
		t.Fatal("explicit delete bypassed existing history FK protection")
	}
	for _, statement := range []string{
		`DELETE FROM conversation_branches WHERE id='branch'`,
		`DELETE FROM conversation_turns WHERE id IN ('retry','promoted')`,
		`INSERT INTO conversation_input_actions(action_key,turn_id,kind,expected_owner,expected_revision,created_at) VALUES('delete','base','delete','owner',1,CURRENT_TIMESTAMP)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	var custody int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_input_custody`).Scan(&custody); err != nil || custody != 0 {
		t.Fatalf("explicit delete left custody: %d %v", custody, err)
	}
	gooseMu.Lock()
	err = migrateDownTo(db, sharedInputCustodyVersion-1)
	gooseMu.Unlock()
	if err == nil {
		t.Fatal("Down discarded action history after explicit input deletion")
	}
	if after := sharedInputObjects(t, db); !reflect.DeepEqual(before, after) {
		t.Fatal("history-only rejected Down removed a guard")
	}
}
