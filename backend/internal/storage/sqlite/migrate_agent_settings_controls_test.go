package sqlite

import (
	"reflect"
	"testing"
)

func TestMigrateManagedSettingsControlsPreservesFactsAndRoundTrips(t *testing.T) {
	const base int64 = 20261029075720713
	db := openMigratedDatabaseCopy(t, base)
	if _, err := db.Exec(`
INSERT INTO projects(id,path,registered_at) VALUES('control','/fake',CURRENT_TIMESTAMP);
INSERT INTO sessions(id,project_id,num,harness,session_mode,activity_last_at,created_at,updated_at)
VALUES('worker','control',1,'codex','chat',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO session_agent_settings_operations(id,session_id,idempotency_key,request_json,phase,created_at,updated_at)
VALUES('legacy-operation','worker','legacy-key','{}','pending',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO session_agent_settings_audit(operation_id,phase,recorded_at) VALUES('legacy-operation','pending',CURRENT_TIMESTAMP);
`); err != nil {
		t.Fatal(err)
	}
	tables := []string{"sessions", "session_agent_settings_operations", "session_agent_settings_audit", "change_log"}
	before := make(map[string][]map[string]any)
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
				t.Fatalf("migration changed %s row count", table)
			}
			for j, row := range before[table] {
				for col, value := range row {
					if !reflect.DeepEqual(after[j][col], value) {
						t.Fatalf("migration rewrote %s.%s", table, col)
					}
				}
			}
		}
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM agent_settings_authority_keys`).Scan(&count); err != nil || count != 0 {
			t.Fatal("migration initialized credentials")
		}
		var code string
		if err := db.QueryRow(`SELECT error_code FROM session_agent_settings_operations WHERE id='legacy-operation'`).Scan(&code); err != nil || code != "" {
			t.Fatal("invented legacy failure code")
		}
		downTo(t, db, base)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity: %s %v", integrity, err)
	}
}
