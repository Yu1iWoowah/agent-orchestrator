package sqlite

import "testing"

func TestGuardianRecoverySnapshotMigrationPreservesExistingFacts(t *testing.T) {
	const previous int64 = 20261101143810287
	const current int64 = 20261101144300192
	db := openMigratedDatabaseCopy(t, previous)
	for _, query := range []string{
		`INSERT INTO projects(id,path,display_name,registered_at) VALUES('project','/synthetic/project','',CURRENT_TIMESTAMP)`,
		`INSERT INTO sessions(id,project_id,num,kind,activity_state,activity_last_at,created_at,updated_at) VALUES('session','project',1,'worker','idle',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`INSERT INTO usage_bindings(id,session_id,harness,native_root_id,state,updated_at) VALUES(1,'session','codex','root','active',CURRENT_TIMESTAMP)`,
		`INSERT INTO guardian_epochs(id,binding_id,session_id,account_id,account_bound_at,first_seen_at) VALUES(1,1,'session','account',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`INSERT INTO guardian_threads(epoch_id,thread_id,parent_id,cli_version,retention_at,first_seen_at,last_seen_at) VALUES(1,'guardian','root','0.166.0',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		`INSERT INTO guardian_requests(cli_version,epoch_id,thread_id,turn_id,response_id,provider_at,first_seen_at,source_generation,source_offset,input_tokens) VALUES('0.166.0',1,'guardian','turn','response',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,1,100,100)`,
		`INSERT INTO guardian_reviews(epoch_id,parent_thread_id,review_id,parent_turn_id,action,status,decision_source,first_seen_at,retention_at,saw_started,saw_completed) VALUES(1,'root','review','turn','networkAccess','approved','agent',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,0,1)`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		t.Helper()
		var input int
		var status string
		if err := db.QueryRow("SELECT input_tokens FROM guardian_requests WHERE response_id='response'").Scan(&input); err != nil || input != 100 {
			t.Fatalf("request changed: %v", err)
		}
		if err := db.QueryRow("SELECT status FROM guardian_reviews WHERE review_id='review'").Scan(&status); err != nil || status != "approved" {
			t.Fatalf("review changed: %v", err)
		}
		var notesColumn int
		if err := db.QueryRow("SELECT count(*) FROM pragma_table_info('attention_answers') WHERE name='notes'").Scan(&notesColumn); err != nil || notesColumn != 1 {
			t.Fatalf("main answer notes column changed: %v", err)
		}
	}
	upTo(t, db, current)
	check()
	var sequence int64
	if err := db.QueryRow("SELECT ingestion_sequence FROM guardian_reviews").Scan(&sequence); err != nil || sequence != 0 {
		t.Fatalf("legacy snapshot missing: %v", err)
	}
	if _, err := db.Exec("DELETE FROM change_log"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO guardian_reviews(epoch_id,parent_thread_id,review_id,parent_turn_id,action,status,decision_source,first_seen_at,retention_at,saw_started,saw_completed) VALUES(1,'root','new-review','turn','execve','inProgress','',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,1,0)`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT ingestion_sequence FROM guardian_reviews WHERE review_id='new-review'").Scan(&sequence); err != nil || sequence != 1 {
		t.Fatalf("new ingestion sequence = %d: %v", sequence, err)
	}
	var cdc int
	if err := db.QueryRow("SELECT count(*) FROM change_log").Scan(&cdc); err != nil || cdc != 1 {
		t.Fatalf("sequence stamping emitted extra CDC: %d %v", cdc, err)
	}
	if _, err := db.Exec(`INSERT INTO guardian_review_pending(epoch_id,parent_thread_id,review_id,parent_turn_id,action,status,decision_source,first_seen_at,retention_at,saw_started,saw_completed) VALUES(1,'root','pending','turn','execve','inProgress','',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,1,0)`); err != nil {
		t.Fatal(err)
	}
	downTo(t, db, previous)
	check()
	upTo(t, db, current)
	check()
	if err := db.QueryRow("SELECT count(*) FROM guardian_review_pending").Scan(&cdc); err != nil || cdc != 0 {
		t.Fatalf("pending schema not recreated: %v", err)
	}
}
