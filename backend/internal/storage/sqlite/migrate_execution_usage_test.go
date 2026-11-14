package sqlite

import (
	"strings"
	"testing"
)

func TestExecutionUsageMigrationPreservesNativeLedger(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 116)
	_, err := db.Exec(`INSERT INTO projects(id,path,registered_at) VALUES('p','/fixture/p',CURRENT_TIMESTAMP);
 INSERT INTO sessions(id,project_id,num,harness,activity_last_at,created_at,updated_at) VALUES('s','p',1,'codex',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
 INSERT INTO usage_bindings(id,session_id,harness,native_root_id,state,updated_at) VALUES(7,'s','codex','root','active',CURRENT_TIMESTAMP);
 INSERT INTO usage_sources(id,binding_id,kind,artifact_path,state,updated_at) VALUES(9,7,'codex_rollout','/fixture/native.jsonl','active',CURRENT_TIMESTAMP);
 INSERT INTO model_usage_events(id,binding_id,usage_source_id,provider_id,billing_provider_id,billing_provider_source,model_id,usage_measurement_kind,input_tokens,cached_input_tokens,uncached_input_tokens,output_tokens,provider_usage_json,source_event_key,estimated_cost_nanos,pricing_version) VALUES(41,7,9,'openai','openai','observed','fixture','native_reported',10,2,8,3,'{"input_tokens":10}','native-key',123,'price-v1');
 INSERT INTO model_usage_events(id,binding_id,usage_source_id,provider_id,model_id,usage_measurement_kind,source_event_key) VALUES(99,7,9,'openai','fixture','native_reported','deleted-native');
 DELETE FROM model_usage_events WHERE id=99;`)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var seq int64
	if err := db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='model_usage_events'`).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	if seq != 99 {
		t.Fatalf("sequence=%d", seq)
	}
	var sequenceRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_sequence WHERE name='model_usage_events'`).Scan(&sequenceRows); err != nil {
		t.Fatal(err)
	}
	if sequenceRows != 1 {
		t.Fatalf("duplicate autoincrement state: %d rows", sequenceRows)
	}
}

func TestExecutionUsageDownRefusesToDiscardReceipts(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261109083558284)
	_, err := db.Exec(`INSERT INTO projects(id,path,registered_at) VALUES('p','/fixture/p',CURRENT_TIMESTAMP);
 INSERT INTO sessions(id,project_id,num,harness,activity_last_at,created_at,updated_at) VALUES('s','p',1,'codex',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
 INSERT INTO usage_execution_sources(id,project_id,session_id,billing_provider,profile_id,profile_revision,profile_digest,delegation_id,attempt_id,request_id,receipt_digest,identity_json) VALUES(1,'p','s','openai','fixture','v1','digest','delegation','attempt','request','receipt','{}');
 INSERT INTO model_usage_events(execution_source_id,provider_id,model_id,usage_measurement_kind,source_event_key,input_tokens,estimated_cost_nanos,pricing_version,created_at) VALUES(1,'openai','fixture','provider_reported','execution-fixture',10,123,'price-v1',CURRENT_TIMESTAMP);`)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := migrationsFS.ReadFile("migrations/20261109083558284_hcg_execution_usage.sql")
	if err != nil {
		t.Fatal(err)
	}
	down := strings.SplitN(string(migration), "-- +goose Down", 2)[1]
	if _, err := db.Exec(down); err == nil {
		t.Fatal("downgrade discarded execution evidence")
	}
	var count, cost int64
	var price string
	if err := db.QueryRow(`SELECT COUNT(*),e.estimated_cost_nanos,e.pricing_version FROM model_usage_events e JOIN usage_execution_sources x ON e.execution_source_id=x.id WHERE x.session_id='s' AND x.attempt_id='attempt'`).Scan(&count, &cost, &price); err != nil {
		t.Fatal(err)
	}
	if count != 1 || cost != 123 || price != "price-v1" {
		t.Fatalf("downgrade rewrote receipt: count=%d cost=%d price=%s", count, cost, price)
	}
}
