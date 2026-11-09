package sqlite

import (
	"testing"
)

func TestExecutionUsageMigrationPreservesNativeLedger(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 116)
	_, err := db.Exec(`INSERT INTO projects(id,path,registered_at) VALUES('p','/fixture/p',CURRENT_TIMESTAMP);
 INSERT INTO sessions(id,project_id,num,harness,activity_last_at,created_at,updated_at) VALUES('s','p',1,'codex',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
 INSERT INTO usage_bindings(id,session_id,harness,native_root_id,state,updated_at) VALUES(7,'s','codex','root','active',CURRENT_TIMESTAMP);
 INSERT INTO usage_sources(id,binding_id,kind,artifact_path,state,updated_at) VALUES(9,7,'codex_rollout','/fixture/native.jsonl','active',CURRENT_TIMESTAMP);
 INSERT INTO model_usage_events(id,binding_id,usage_source_id,provider_id,billing_provider_id,billing_provider_source,model_id,usage_measurement_kind,input_tokens,cached_input_tokens,uncached_input_tokens,output_tokens,provider_usage_json,source_event_key,estimated_cost_nanos,pricing_version) VALUES(41,7,9,'openai','openai','observed','fixture','native_reported',10,2,8,3,'{"input_tokens":10}','native-key',123,'price-v1');`)
	if err != nil {
		t.Fatal(err)
	}
	const query = `SELECT json_array(id,binding_id,usage_source_id,provider_id,billing_provider_id,billing_provider_source,model_id,usage_measurement_kind,input_tokens,cached_input_tokens,uncached_input_tokens,output_tokens,provider_usage_json,source_event_key,estimated_cost_nanos,pricing_version) FROM model_usage_events ORDER BY id`
	var before, after string
	if err := db.QueryRow(query).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(query).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("native facts changed: %s -> %s", before, after)
	}
	// Existing native constraints still reject auxiliary-shaped artifacts.
	if _, err := db.Exec(`INSERT INTO usage_sources(binding_id,kind,artifact_path,state,updated_at) VALUES(7,'hcg_execution','','active',CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("native source constraints weakened")
	}
	if _, err := db.Exec(`INSERT INTO model_usage_events(provider_id,model_id,usage_measurement_kind,source_event_key) VALUES('openai','fixture','provider_reported','orphan')`); err == nil {
		t.Fatal("accepted event without provenance")
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("migration left broken foreign keys")
	}
	var seq int64
	if err := db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='model_usage_events'`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 41 {
		t.Fatalf("sequence=%d", seq)
	}
}
