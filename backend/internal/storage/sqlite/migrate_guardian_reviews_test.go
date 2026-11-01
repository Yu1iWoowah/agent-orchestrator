package sqlite

import (
	"testing"
	"time"
)

func TestGuardianReviewMigrationUpgradeDowngradePreservesFacts(t *testing.T) {
	const previous int64 = 20261031143851898
	const current int64 = 20261101121152789
	db := openMigratedDatabaseCopy(t, previous)
	now := time.Now().UTC()
	for _, query := range []string{
		`INSERT INTO projects(id,path,display_name,registered_at) VALUES('project','/synthetic/project','',CURRENT_TIMESTAMP)`,
		`INSERT INTO sessions(id,project_id,num,kind,activity_state,activity_last_at,created_at,updated_at) VALUES('session','project',1,'worker','idle',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`INSERT INTO usage_bindings(id,session_id,harness,native_root_id,state,updated_at) VALUES(1,'session','codex','root','active',CURRENT_TIMESTAMP)`,
		`INSERT INTO usage_sources(id,binding_id,kind,artifact_path,state,updated_at) VALUES(1,1,'codex_rollout','/synthetic/rollout.jsonl','active',CURRENT_TIMESTAMP)`,
		`INSERT INTO model_usage_events(binding_id,usage_source_id,provider_id,model_id,usage_measurement_kind,source_event_key,estimated_cost_nanos) VALUES(1,1,'openai','model','native_reported','ledger-key',123)`,
		`INSERT INTO guardian_epochs(id,binding_id,session_id,account_id,account_bound_at,first_seen_at) VALUES(1,1,'session','account',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`INSERT INTO guardian_threads(epoch_id,thread_id,parent_id,cli_version,retention_at,first_seen_at,last_seen_at) VALUES(1,'guardian','root','0.166.0',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`INSERT INTO guardian_requests(cli_version,epoch_id,thread_id,turn_id,response_id,provider_at,first_seen_at,source_generation,source_offset,input_tokens,cached_tokens) VALUES('0.166.0',1,'guardian','turn','response',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,1,100,100,20)`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		t.Helper()
		var key string
		var cost, input, cached int64
		if err := db.QueryRow("SELECT source_event_key,estimated_cost_nanos FROM model_usage_events").Scan(&key, &cost); err != nil || key != "ledger-key" || cost != 123 {
			t.Fatalf("ledger changed: %v", err)
		}
		if err := db.QueryRow("SELECT input_tokens,cached_tokens FROM guardian_requests").Scan(&input, &cached); err != nil || input != 100 || cached != 20 {
			t.Fatalf("request changed: %v", err)
		}
	}
	upTo(t, db, current)
	check()
	if _, err := db.Exec(`INSERT INTO guardian_reviews(epoch_id,parent_thread_id,review_id,parent_turn_id,action,status,decision_source,first_seen_at,retention_at,saw_started,saw_completed) VALUES(1,'root','review','turn','networkAccess','approved','agent',?,?,0,1)`, now, now); err != nil {
		t.Fatal(err)
	}
	downTo(t, db, previous)
	check()
	upTo(t, db, current)
	check()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM guardian_reviews").Scan(&count); err != nil || count != 0 {
		t.Fatalf("review migration not recreated: %v", err)
	}
}
