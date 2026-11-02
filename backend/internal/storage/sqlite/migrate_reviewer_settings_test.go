package sqlite

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/pressly/goose/v3"
)

const reviewerSettingsVersion int64 = 20261030173049692

func reviewerSettingsMigrationFixture(t *testing.T) *sql.DB {
	t.Helper()
	db := openMigratedDatabaseCopy(t, reviewerSettingsVersion-1)
	_, err := db.Exec(`INSERT INTO projects(id,path,registered_at) VALUES('p','/fake',CURRENT_TIMESTAMP);
INSERT INTO sessions(id,project_id,num,harness,activity_last_at,created_at,updated_at) VALUES('worker','p',1,'codex',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO review(id,session_id,project_id,harness,agent_session_id,reviewer_launch_id,created_at,updated_at) VALUES('review','worker','p','codex','native','launch',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO reviewer_agent_accounts(review_id,provider,account_id,resolution_source,bound_at) VALUES('review','codex','account-A','explicit',CURRENT_TIMESTAMP);
INSERT INTO review_run(id,review_id,session_id,harness,pr_url,target_sha,status,verdict,created_at) VALUES('run','review','worker','codex','https://example.invalid/pull/1','head','complete','approved',CURRENT_TIMESTAMP);`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestReviewerSettingsMigrationDoesNotInventHistoricalSelections(t *testing.T) {
	db := reviewerSettingsMigrationFixture(t)
	tables := []string{"sessions", "review", "review_run", "reviewer_agent_accounts", "change_log"}
	before := map[string][]map[string]any{}
	for _, table := range tables {
		before[table] = migrationTableFacts(t, db, table)
	}
	upTo(t, db, reviewerSettingsVersion)
	for _, table := range tables {
		after := migrationTableFacts(t, db, table)
		if table == "review_run" {
			for _, row := range after {
				if row["agent_settings_json"] != nil || row["agent_settings_revision"] != nil {
					t.Fatal("invented historical selection")
				}
				delete(row, "agent_settings_json")
				delete(row, "agent_settings_revision")
			}
		}
		if !reflect.DeepEqual(before[table], after) {
			t.Fatalf("migration changed existing %s facts: before=%+v after=%+v", table, before[table], after)
		}
	}
}

func TestReviewerSettingsDowngradeCannotErasePreferencesOrPassSnapshot(t *testing.T) {
	for _, selection := range []string{"preference", "pass"} {
		t.Run(selection, func(t *testing.T) {
			db := reviewerSettingsMigrationFixture(t)
			upTo(t, db, reviewerSettingsVersion)
			query := `INSERT INTO reviewer_agent_settings(review_id,settings_json,created_at,updated_at) VALUES('review','{"model":"m","effort":"high","fastMode":"off"}',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`
			if selection == "pass" {
				query = `UPDATE review_run SET agent_settings_json='{"model":"m","effort":"high","fastMode":"off"}',agent_settings_revision=0 WHERE id='run'`
			}
			if _, err := db.Exec(query); err != nil {
				t.Fatal(err)
			}
			gooseMu.Lock()
			goose.SetBaseFS(migrationsFS)
			err := migrateDownTo(db, reviewerSettingsVersion-1)
			gooseMu.Unlock()
			if err == nil {
				t.Fatal("downgrade erased reviewer settings evidence")
			}
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('review_run') WHERE name='agent_settings_json'`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("failed downgrade partially changed schema: %d %v", count, err)
			}
		})
	}
}
