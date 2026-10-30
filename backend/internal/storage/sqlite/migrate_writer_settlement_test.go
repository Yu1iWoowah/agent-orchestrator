package sqlite

import (
	"reflect"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestWriterSettlementMigrationPreservesFactsAndRejectsHistoryLoss(t *testing.T) {
	const version int64 = 20261030090402684
	// A snapshot immediately before the additive table has no stop receipts.
	db := openMigratedDatabaseCopy(t, version-1)
	if _, err := db.Exec(`INSERT INTO projects(id,path,registered_at) VALUES('settle','/fake',CURRENT_TIMESTAMP);
INSERT INTO sessions(id,project_id,num,harness,session_mode,activity_last_at,created_at,updated_at)
VALUES('worker','settle',1,'codex','tui',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO session_agent_settings_operations(id,session_id,idempotency_key,request_json,phase,created_at,updated_at)
VALUES('settled-op','worker','settlement','{}','cancelled',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);`); err != nil {
		t.Fatal(err)
	}
	before := migrationTableFacts(t, db, "sessions")
	for range 2 {
		upTo(t, db, version)
		if !reflect.DeepEqual(before, migrationTableFacts(t, db, "sessions")) {
			t.Fatal("additive receipt migration rewrote existing session")
		}
		downTo(t, db, version-1)
	}
	upTo(t, db, version)
	if _, err := db.Exec(`INSERT INTO session_agent_settings_writer_settlements(operation_id,operation_revision,recorded_at,request_id) VALUES('settled-op',3,CURRENT_TIMESTAMP,'operator-request')`); err != nil {
		t.Fatal(err)
	}
	gooseMu.Lock()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	err := goose.SetDialect("sqlite3")
	if err == nil {
		err = goose.DownTo(db, "migrations", version-1)
	}
	gooseMu.Unlock()
	if err == nil {
		t.Fatal("downgrade erased operator stop evidence")
	}
	var actor, request string
	if err := db.QueryRow(`SELECT actor_kind,request_id FROM session_agent_settings_writer_settlements WHERE operation_id='settled-op'`).Scan(&actor, &request); err != nil || actor != "local_operator_unverified" || request != "operator-request" {
		t.Fatalf("receipt lost after rejected downgrade: %q %q %v", actor, request, err)
	}
	if !reflect.DeepEqual(before, migrationTableFacts(t, db, "sessions")) {
		t.Fatal("rejected downgrade changed session facts")
	}
}
