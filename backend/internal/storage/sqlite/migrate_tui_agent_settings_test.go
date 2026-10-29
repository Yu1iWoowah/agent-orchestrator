package sqlite

import (
	"reflect"
	"testing"

	"github.com/pressly/goose/v3"
)

const tuiSettingsBaseVersion int64 = 20261029135418896

func TestMigrateTUISettingsPreservesEveryExistingFactAndRoundTrips(t *testing.T) {
	db := openMigratedDatabaseCopy(t, tuiSettingsBaseVersion)
	if _, err := db.Exec(`
 INSERT INTO projects(id,path,registered_at) VALUES('tui-settings','/fake',CURRENT_TIMESTAMP);
 INSERT INTO sessions(id,project_id,num,harness,session_mode,activity_last_at,created_at,updated_at)
 VALUES('tui-worker','tui-settings',1,'codex','tui',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP),
 ('chat-worker','tui-settings',2,'claude-code','chat',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
 INSERT INTO session_interface_transitions(id,session_id,source_mode,target_mode,policy,history_policy,phase,native_conversation_id,created_at,updated_at,completed_at)
 VALUES('old-handoff','tui-worker','chat','tui','drain','strict','completed','native',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
 `); err != nil {
		t.Fatal(err)
	}
	tables := []string{"sessions", "session_interface_transitions", "change_log"}
	before := map[string][]map[string]any{}
	for _, table := range tables {
		before[table] = migrationTableFacts(t, db, table)
	}
	for i := 0; i < 2; i++ {
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
		for _, table := range tables {
			after := migrationTableFacts(t, db, table)
			if len(after) != len(before[table]) {
				t.Fatalf("%s row count changed", table)
			}
			for j, row := range before[table] {
				for col, value := range row {
					if !reflect.DeepEqual(after[j][col], value) {
						t.Fatalf("migration changed %s.%s: before=%v after=%v", table, col, value, after[j][col])
					}
				}
			}
		}
		var marker string
		if err := db.QueryRow("SELECT settings_operation_id FROM session_interface_transitions WHERE id='old-handoff'").Scan(&marker); err != nil || marker != "" {
			t.Fatalf("invented settings owner: %q %v", marker, err)
		}
		downTo(t, db, tuiSettingsBaseVersion)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity=%s %v", integrity, err)
	}
}

func TestMigrateTUISettingsRecoveryOwnsGateAndHistoryCannotBeDiscarded(t *testing.T) {
	for _, phase := range []string{"completed", "recovery_required"} {
		t.Run(phase, func(t *testing.T) {
			db := openMigratedDatabaseCopy(t, tuiSettingsBaseVersion)
			if err := migrate(db); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO projects(id,path,registered_at) VALUES('p','/fake',CURRENT_TIMESTAMP);
  INSERT INTO sessions(id,project_id,num,harness,session_mode,activity_last_at,created_at,updated_at) VALUES('worker','p',1,'codex','tui',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
  INSERT INTO session_agent_settings_operations(id,session_id,idempotency_key,request_json,phase,apply_mode,created_at,updated_at) VALUES('restart','worker','key','{}','pending','restart',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);`); err != nil {
				t.Fatal(err)
			}
			insert := `INSERT INTO session_interface_transitions(id,settings_operation_id,session_id,source_mode,target_mode,policy,history_policy,phase,created_at,updated_at) VALUES(?,?,'worker','tui','tui','drain','strict',?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`
			if _, err := db.Exec(insert, "orphan", "orphan", phase); err == nil {
				t.Fatal("same-mode restart admitted without a settings owner")
			}
			if _, err := db.Exec(insert, "unexplained", "", phase); err == nil {
				t.Fatal("same-mode handoff admitted without a restart reason")
			}
			if _, err := db.Exec(insert, "restart", "restart", phase); err != nil {
				t.Fatal(err)
			}
			if phase == "recovery_required" {
				if _, err := db.Exec(`INSERT INTO session_interface_transitions(id,session_id,source_mode,target_mode,policy,history_policy,phase,created_at,updated_at) VALUES('other','worker','tui','chat','drain','strict','requested',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err == nil {
					t.Fatal("recovery lost one-active-controller uniqueness")
				}
			}
			gooseMu.Lock()
			goose.SetBaseFS(migrationsFS)
			goose.SetLogger(goose.NopLogger())
			err := goose.SetDialect("sqlite3")
			if err == nil {
				err = goose.DownTo(db, "migrations", tuiSettingsBaseVersion)
			}
			gooseMu.Unlock()
			if err == nil {
				t.Fatal("downgrade discarded a physical handoff or its history")
			}
			var count int
			if err := db.QueryRow("SELECT COUNT(*) FROM session_interface_transitions WHERE id='restart'").Scan(&count); err != nil || count != 1 {
				t.Fatalf("failed downgrade lost journal: %d %v", count, err)
			}
		})
	}
}
