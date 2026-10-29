package sqlite

import (
	"database/sql"
	"github.com/pressly/goose/v3"
	"reflect"
	"testing"
)

func TestReviewerExecutionHistoryPreservesOldBindingsAndRejectsUnsafeDowngrade(t *testing.T) {
	db := openMigratedDatabaseCopyNoForeignKeys(t, 20261029163259629)
	// Use valid parents: the migration restores enforcement after its atomic
	// table rebuild, while this snapshot helper starts with FK checks disabled.
	if _, err := db.Exec(`INSERT INTO projects(id,path,registered_at) VALUES('p','/fake/project',CURRENT_TIMESTAMP); INSERT INTO sessions(id,project_id,num,activity_last_at,created_at,updated_at) VALUES('worker','p',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO review(id,session_id,project_id,harness,agent_session_id,is_archived,created_at,updated_at) VALUES('old-review','worker','p','codex','old-native',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP); INSERT INTO reviewer_agent_accounts(review_id,provider,account_id,resolution_source,bound_at) VALUES('old-review','codex','old-A','explicit',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	beforeTriggers := reviewerTriggerSnapshot(t, db)
	upTo(t, db, 20261029165951329)
	if after := reviewerTriggerSnapshot(t, db); !reflect.DeepEqual(beforeTriggers, after) {
		t.Fatalf("table rebuild changed triggers: before=%v after=%v", beforeTriggers, after)
	}
	for _, table := range []string{"review_run", "reviewer_agent_accounts", "conversations"} {
		var count int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_list(?) WHERE "table"='review'`, table).Scan(&count); err != nil || count == 0 {
			t.Fatalf("%s FK was rewritten away from review: %d %v", table, count, err)
		}
	}
	var bad int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&bad); err != nil || bad != 0 {
		t.Fatalf("table rebuild broke child ownership: violations=%d %v", bad, err)
	}
	if _, err := db.Exec(`INSERT INTO review(id,session_id,project_id,harness,is_archived,created_at,updated_at) VALUES('new-review','worker','p','codex',0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal("explicit rebuild could not retain archived execution", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM reviewer_agent_accounts WHERE review_id='old-review' AND account_id='old-A'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("historical binding was discarded: %d %v", count, err)
	}
	if _, err := db.Exec(`INSERT INTO review(id,session_id,project_id,harness,is_archived,created_at,updated_at) VALUES('second-active','worker','p','codex',0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("two current reviewer executions admitted")
	}
	gooseMu.Lock()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	err := goose.SetDialect("sqlite3")
	if err == nil {
		err = goose.DownTo(db, "migrations", 20261029163259629)
	}
	gooseMu.Unlock()
	if err == nil {
		t.Fatal("downgrade discarded reviewer execution history")
	}
	if err := db.QueryRow(`SELECT count(*) FROM review WHERE session_id='worker'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("failed downgrade changed history: %d %v", count, err)
	}
}

func reviewerTriggerSnapshot(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT name,sql FROM sqlite_master WHERE type='trigger' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, sql string
		if err := rows.Scan(&name, &sql); err != nil {
			t.Fatal(err)
		}
		out[name] = sql
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
