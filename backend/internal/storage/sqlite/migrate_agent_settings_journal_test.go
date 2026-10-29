package sqlite

import (
	"reflect"
	"testing"
)

func TestMigrateAgentSettingsJournalPreservesExistingFacts(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261028204037417)
	if _, err := db.Exec("INSERT INTO projects(id,path,registered_at) VALUES('journal','/fake',CURRENT_TIMESTAMP)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO sessions(id,project_id,num,harness,activity_last_at,created_at,updated_at,session_mode) VALUES('legacy','journal',1,'claude-code',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'chat')"); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"pending", "applied"} {
		if _, err := db.Exec("INSERT INTO session_agent_settings_operations(id,session_id,idempotency_key,request_json,phase,created_at,updated_at) VALUES(?,'legacy',?,'{}',?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)", phase, phase, phase); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO session_agent_settings_audit(operation_id,phase,recorded_at) VALUES(?,?,CURRENT_TIMESTAMP)", phase, phase); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func(table string) []map[string]any {
		t.Helper()
		rows, err := db.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var result []map[string]any
		for rows.Next() {
			values := make([]any, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				t.Fatal(err)
			}
			row := make(map[string]any, len(columns))
			for i, col := range columns {
				row[col] = values[i]
			}
			result = append(result, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	tables := []string{"sessions", "session_agent_settings_operations", "session_agent_settings_audit", "change_log"}
	before := make(map[string][]map[string]any)
	for _, table := range tables {
		before[table] = snapshot(table)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		after := snapshot(table)
		if len(after) != len(before[table]) {
			t.Fatalf("%s row count changed", table)
		}
		for i, row := range before[table] {
			for col, value := range row {
				if !reflect.DeepEqual(after[i][col], value) {
					t.Errorf("%s[%d].%s changed: before=%#v after=%#v", table, i, col, value, after[i][col])
				}
			}
		}
	}
	for _, row := range snapshot("session_agent_settings_operations") {
		if row["apply_mode"] != "after-turn" || row["checkpoint"] != "queued" || row["operation_revision"] != int64(0) || row["error_category"] != "" || row["request_id"] != "" || row["started_at"] != nil || row["finished_at"] != nil {
			t.Fatalf("invented legacy facts: %#v", row)
		}
	}
}
