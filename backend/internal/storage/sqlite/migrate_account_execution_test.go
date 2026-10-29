package sqlite

import (
	"testing"

	"github.com/pressly/goose/v3"
)

func TestExecutionBindingMigrationPreservesLegacyReviewerAndGuardsDowngrade(t *testing.T) {
	for _, guard := range []string{"legacy", "reviewer", "session", "automation", "project"} {
		t.Run(guard, func(t *testing.T) {
			db := openMigratedDatabaseCopyNoForeignKeys(t, 20261029033801026)
			if _, err := db.Exec(`INSERT INTO review(id,session_id,project_id,harness,created_at,updated_at) VALUES('old-review','old-worker','p','codex',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
				t.Fatal(err)
			}
			upTo(t, db, 20261029104125598)
			var count, legacy int
			if err := db.QueryRow(`SELECT count(*),sum(account_id='device-'||provider AND resolution_source='legacy') FROM reviewer_agent_accounts WHERE review_id='old-review'`).Scan(&count, &legacy); err != nil || count != 2 || legacy != 2 {
				t.Fatalf("legacy reviewer identities: count=%d preserved=%d err=%v", count, legacy, err)
			}
			var statement string
			switch guard {
			case "reviewer":
				statement = `UPDATE reviewer_agent_accounts SET account_id='private-C',resolution_source='role' WHERE provider='codex'`
			case "session":
				statement = `INSERT INTO session_agent_accounts(session_id,provider,account_id,bound_at,resolution_source) VALUES('old-worker','codex','private-A',CURRENT_TIMESTAMP,'explicit')`
			case "automation":
				statement = `INSERT INTO automations(id,project_id,display_name,prompt,kind,rrule_text,timezone,enabled,next_run_at,created_at,updated_at,agent_account_id) VALUES('schedule','p','Fake','Fake','worker','FREQ=DAILY','UTC',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'private-A')`
			case "project":
				statement = `INSERT INTO projects(id,path,registered_at,config) VALUES('p','/fake/project',CURRENT_TIMESTAMP,'{"worker":{"agent":"codex","agentAccountId":"private-A"}}')`
			}
			if statement != "" {
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			gooseMu.Lock()
			goose.SetBaseFS(migrationsFS)
			goose.SetLogger(goose.NopLogger())
			err := goose.SetDialect("sqlite3")
			if err == nil {
				err = goose.DownTo(db, "migrations", 20261029033801026)
			}
			gooseMu.Unlock()
			if guard == "legacy" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil {
				t.Fatal("downgrade discarded local account references")
			}
			if err := db.QueryRow(`SELECT count(*) FROM reviewer_agent_accounts WHERE review_id='old-review'`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("failed downgrade changed bindings: %d %v", count, err)
			}
		})
	}
}
