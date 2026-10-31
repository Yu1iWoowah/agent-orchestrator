package sqlite

import (
	"testing"
	"time"
)

func TestGuardianMigrationUpgradeDowngradePreservesLedger(t *testing.T) {
	const previous int64 = 20261030173049692
	db := openMigratedDatabaseCopy(t, previous)
	now := time.Now().UTC()
	statements := []string{
		`INSERT INTO projects(id,path,display_name,registered_at) VALUES('guardian-migration','/synthetic/guardian','',?)`,
		`INSERT INTO sessions(id,project_id,num,kind,activity_state,activity_last_at,created_at,updated_at) VALUES('guardian-session','guardian-migration',1,'worker','idle',?,?,?)`,
		`INSERT INTO usage_bindings(id,session_id,harness,native_root_id,state,updated_at) VALUES(1,'guardian-session','codex','root','active',?)`,
		`INSERT INTO usage_sources(id,binding_id,kind,artifact_path,state,updated_at) VALUES(1,1,'codex_rollout','/synthetic/rollout.jsonl','active',?)`,
		`INSERT INTO model_usage_events(binding_id,usage_source_id,provider_id,model_id,usage_measurement_kind,source_event_key,estimated_cost_nanos) VALUES(1,1,'openai','synthetic','native_reported','old-ledger-key',123)`,
	}
	for i, statement := range statements {
		var args []any
		switch i {
		case 0, 2, 3:
			args = []any{now}
		case 1:
			args = []any{now, now, now}
		}
		if _, err := db.Exec(statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	assertLedger := func() {
		t.Helper()
		var id, cost int64
		var key string
		if err := db.QueryRow(`SELECT id,source_event_key,estimated_cost_nanos FROM model_usage_events`).Scan(&id, &key, &cost); err != nil || id != 1 || key != "old-ledger-key" || cost != 123 {
			t.Fatalf("ledger changed: id=%d key=%s cost=%d err=%v", id, key, cost, err)
		}
	}
	upTo(t, db, 20261031143851898)
	assertLedger()
	var enabled int
	if err := db.QueryRow(`SELECT enabled FROM guardian_project_policy WHERE project_id='guardian-migration'`).Scan(&enabled); err != nil || enabled != 0 {
		t.Fatalf("migration opted in existing project: enabled=%d err=%v", enabled, err)
	}
	if _, err := db.Exec(`UPDATE projects SET config='{"guardianObservability":{"enabled":true}}' WHERE id='guardian-migration'`); err != nil {
		t.Fatal(err)
	}
	downTo(t, db, previous)
	assertLedger()
	upTo(t, db, 20261031143851898)
	assertLedger()
	if err := db.QueryRow(`SELECT enabled FROM guardian_project_policy WHERE project_id='guardian-migration'`).Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("explicit config lost on re-upgrade: enabled=%d err=%v", enabled, err)
	}
}
