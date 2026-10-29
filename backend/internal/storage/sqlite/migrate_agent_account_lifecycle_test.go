package sqlite

import (
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

func TestLifecycleMigrationPreservesMetadataAndHistoricalBindings(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261028193324307)
	now := time.Now().UTC()
	if _, e := db.Exec(`INSERT INTO projects(id,path,display_name,registered_at) VALUES('lifecycle','/fake/lifecycle','Fake',?)`, now); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec(`INSERT INTO sessions(id,project_id,num,kind,activity_state,activity_last_at,is_terminated,created_at,updated_at) VALUES('bound','lifecycle',1,'worker','idle',?,1,?,?)`, now, now, now); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec(`INSERT INTO agent_accounts(id,provider,home,email,plan,created_at,profile_kind) VALUES('saved','claude','/fake/original-spelling','fake@example.invalid','fake-plan',?,'external-linked')`, now); e != nil {
		t.Fatal(e)
	}
	if _, e := db.Exec(`INSERT INTO session_agent_accounts(session_id,provider,account_id,bound_at) VALUES('bound','claude','saved',?)`, now); e != nil {
		t.Fatal(e)
	}
	upTo(t, db, 20261029033801026)
	var home, email, kind, label string
	var disabled, retired any
	if e := db.QueryRow(`SELECT home,email,profile_kind,label,disabled_at,retired_at FROM agent_accounts WHERE id='saved'`).Scan(&home, &email, &kind, &label, &disabled, &retired); e != nil {
		t.Fatal(e)
	}
	if home != "/fake/original-spelling" || email != "fake@example.invalid" || kind != "external-linked" || label != "" || disabled != nil || retired != nil {
		t.Fatal("migration changed existing identity/storage spelling")
	}
	if _, e := db.Exec(`UPDATE agent_accounts SET label='Fake label',retired_at=?,disabled_at=? WHERE id='saved'`, now, now); e != nil {
		t.Fatal(e)
	}
	var binding string
	if e := db.QueryRow(`SELECT account_id FROM session_agent_accounts WHERE session_id='bound'`).Scan(&binding); e != nil || binding != "saved" {
		t.Fatal("tombstone lost historical binding")
	}
	var payload string
	if e := db.QueryRow(`SELECT payload FROM change_log WHERE json_extract(payload,'$.kind')='agent_accounts' ORDER BY seq DESC LIMIT 1`).Scan(&payload); e != nil {
		t.Fatalf("metadata CDC query: %v", e)
	}
	if payload != `{"kind":"agent_accounts","accountId":"saved"}` {
		t.Fatalf("unexpected metadata CDC projection: %s", payload)
	}
	if _, e := db.Exec(`INSERT INTO agent_account_operations(id,account_id,provider,action,phase,reason_code,created_at,updated_at) VALUES('bad','saved','claude','logout','logging-out','fake-provider-secret',?,?)`, now, now); e == nil {
		t.Fatal("journal accepted arbitrary provider error text")
	}
	gooseMu.Lock()
	rollbackErr := goose.DownTo(db, "migrations", 20261028193324307)
	gooseMu.Unlock()
	if rollbackErr == nil {
		t.Fatal("downgrade erased durable lifecycle facts")
	}
	if e := db.QueryRow(`SELECT account_id FROM session_agent_accounts WHERE session_id='bound'`).Scan(&binding); e != nil || binding != "saved" {
		t.Fatal("rollback erased prior binding")
	}
	if e := db.QueryRow(`SELECT label,retired_at FROM agent_accounts WHERE id='saved'`).Scan(&label, &retired); e != nil || label != "Fake label" || retired == nil {
		t.Fatal("rejected rollback lost tombstone metadata")
	}

}
