package sqlite

import (
	"testing"

	"github.com/pressly/goose/v3"
)

func TestLegacyConflictMigrationPreservesJournalAndGuardsDowngrade(t *testing.T) {
	db := openMigratedDatabaseCopyNoForeignKeys(t, 20261029232658295)
	_, err := db.Exec(`INSERT INTO projects(id,path,registered_at)VALUES('p','/fake/project',CURRENT_TIMESTAMP);
 INSERT INTO agent_account_operations(id,account_id,provider,action,phase,outcome,reason_code,terminal_id,result_account_id,was_disabled,created_at,updated_at)
 VALUES('old','A','codex','login-again','closing','mismatch','account_cleanup_unconfirmed','shellterm-old','existing',1,'2026-10-28T01:02:03Z','2026-10-28T01:02:04Z')`)
	if err != nil {
		t.Fatal(err)
	}
	query := `SELECT json_object('id',id,'account',account_id,'provider',provider,'action',action,'phase',phase,'outcome',outcome,'reason',reason_code,'terminal',terminal_id,'result',result_account_id,'disabled',was_disabled,'created',created_at,'updated',updated_at)FROM agent_account_operations WHERE id='old'`
	var before, after string
	if err = db.QueryRow(query).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		upTo(t, db, 20261030101902833)
		if err = db.QueryRow(query).Scan(&after); err != nil || after != before {
			t.Fatalf("upgrade changed recovery facts: %v", err)
		}
		downTo(t, db, 20261029232658295)
		if err = db.QueryRow(query).Scan(&after); err != nil || after != before {
			t.Fatalf("downgrade changed recovery facts: %v", err)
		}
	}
	upTo(t, db, 20261030101902833)
	var countBefore, countAfter int
	_ = db.QueryRow(`SELECT count(*)FROM change_log`).Scan(&countBefore)
	_, err = db.Exec(`INSERT INTO agent_account_operations(id,account_id,provider,action,phase,outcome,created_at,updated_at) VALUES('conflict','B','codex','login','closing','legacy-conflict',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.QueryRow(`SELECT count(*)FROM change_log`).Scan(&countAfter)
	if countAfter != countBefore+1 {
		t.Fatal("insert CDC lost or duplicated")
	}
	_, err = db.Exec(`INSERT INTO agent_account_operations(id,account_id,provider,action,phase,created_at,updated_at)VALUES('blocked','B','codex','login','pending',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	if err == nil {
		t.Fatal("active operation uniqueness lost")
	}
	_, err = db.Exec(`UPDATE agent_account_operations SET phase='complete'WHERE id='conflict'`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.QueryRow(`SELECT count(*)FROM change_log`).Scan(&countAfter)
	if countAfter != countBefore+2 {
		t.Fatal("update CDC lost or duplicated")
	}
	gooseMu.Lock()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	err = goose.SetDialect("sqlite3")
	if err == nil {
		err = migrateDownTo(db, 20261029232658295)
	}
	gooseMu.Unlock()
	if err == nil {
		t.Fatal("unsafe downgrade discarded conflict outcome")
	}
	var outcome string
	if err = db.QueryRow(`SELECT outcome FROM agent_account_operations WHERE id='conflict'`).Scan(&outcome); err != nil || outcome != "legacy-conflict" {
		t.Fatal("failed rollback changed durable outcome", err)
	}
}
