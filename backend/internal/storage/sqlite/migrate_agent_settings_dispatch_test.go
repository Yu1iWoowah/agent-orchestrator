package sqlite

import (
	"database/sql"
	"reflect"
	"testing"
)

func migrationTableFacts(t *testing.T, db *sql.DB, table string) []map[string]any {
	t.Helper()
	rows, err := db.Query("SELECT * FROM " + table + " ORDER BY rowid")
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
		values, dest := make([]any, len(columns)), make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		row := make(map[string]any, len(columns))
		for i, col := range columns {
			value := values[i]
			if raw, ok := value.([]byte); ok {
				value = append([]byte(nil), raw...)
			}
			row[col] = value
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestMigrateDispatchSnapshotsPreservesEveryLegacyFact(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261029011627701)
	if _, err := db.Exec(`
INSERT INTO projects(id,path,registered_at) VALUES('dispatch','/fake',CURRENT_TIMESTAMP);
INSERT INTO sessions(id,project_id,num,harness,session_mode,activity_last_at,created_at,updated_at)
VALUES('legacy','dispatch',1,'codex','chat',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO conversations(id,scope,project_id,session_id,current_session_id,created_at,updated_at)
VALUES('conversation','session','dispatch','legacy','legacy',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO conversation_turns(id,conversation_id,handled_by_session_id,state,requested_at)
VALUES('turn','conversation','legacy','completed',CURRENT_TIMESTAMP);
INSERT INTO conversation_messages(id,conversation_id,turn_id,sequence,role,text,origin,created_at,updated_at)
VALUES('message','conversation','turn',1,'user','preserve exactly','human',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
`); err != nil {
		t.Fatal(err)
	}
	tables := []string{"sessions", "conversations", "conversation_turns", "conversation_messages", "change_log"}
	before := make(map[string][]map[string]any)
	for _, table := range tables {
		before[table] = migrationTableFacts(t, db, table)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		after := migrationTableFacts(t, db, table)
		if len(after) != len(before[table]) {
			t.Fatalf("%s row count changed", table)
		}
		for i, row := range before[table] {
			for col, value := range row {
				if !reflect.DeepEqual(after[i][col], value) {
					t.Errorf("migration rewrote %s[%d].%s: %#v -> %#v", table, i, col, value, after[i][col])
				}
			}
		}
	}
	var snapshot string
	if err := db.QueryRow("SELECT agent_settings_snapshot_json FROM conversation_turns WHERE id='turn'").Scan(&snapshot); err != nil || snapshot != "" {
		t.Fatalf("invented historical settings: %q %v", snapshot, err)
	}
}

func TestMigrateExecutionPreimagesPreservesExistingJournalAndSessionFacts(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261029043347587)
	if _, err := db.Exec(`
INSERT INTO projects(id,path,registered_at) VALUES('execution','/fake',CURRENT_TIMESTAMP);
INSERT INTO sessions(id,project_id,num,harness,session_mode,activity_last_at,created_at,updated_at)
VALUES('legacy','execution',1,'codex','chat',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO session_agent_settings_operations(id,session_id,idempotency_key,request_json,phase,created_at,updated_at)
VALUES('operation','legacy','key','{"baseRevision":0,"source":{"model":"","effort":"","fastMode":"default"},"target":{"model":"next","effort":"low","fastMode":"off"}}','pending',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO session_agent_settings_audit(operation_id,phase,recorded_at) VALUES('operation','pending',CURRENT_TIMESTAMP);
`); err != nil {
		t.Fatal(err)
	}
	tables := []string{"sessions", "session_agent_settings_operations", "session_agent_settings_audit", "change_log"}
	before := make(map[string][]map[string]any)
	for _, table := range tables {
		before[table] = migrationTableFacts(t, db, table)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		after := migrationTableFacts(t, db, table)
		if len(after) != len(before[table]) {
			t.Fatalf("%s row count changed", table)
		}
		for i, row := range before[table] {
			for col, value := range row {
				if !reflect.DeepEqual(after[i][col], value) {
					t.Errorf("migration rewrote %s[%d].%s", table, i, col)
				}
			}
		}
	}
	var execution string
	if err := db.QueryRow("SELECT execution_json FROM session_agent_settings_operations WHERE id='operation'").Scan(&execution); err != nil || execution != "" {
		t.Fatalf("legacy journal received an invented provider preimage: %q %v", execution, err)
	}
}
