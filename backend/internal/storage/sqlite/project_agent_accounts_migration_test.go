package sqlite

import (
	"database/sql"
	"strings"
	"testing"
)

const beforeRemoveProjectAgentAccounts int64 = 20261114020404081

func insertProjectConfig(t *testing.T, db *sql.DB, id string, config any, archived bool) {
	t.Helper()
	archivedAt := any(nil)
	if archived {
		archivedAt = "2026-11-01 00:00:00"
	}
	if _, err := db.Exec(`INSERT INTO projects(id,path,registered_at,config,archived_at) VALUES(?,?,CURRENT_TIMESTAMP,?,?)`, id, "/fake/"+id, config, archivedAt); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveProjectAgentAccountsDeletesOnlyThatKey(t *testing.T) {
	db := openMigratedDatabaseCopy(t, beforeRemoveProjectAgentAccounts)
	plain := `{"agentRules":"keep"}`
	insertProjectConfig(t, db, "null", nil, false)
	insertProjectConfig(t, db, "plain", plain, false)
	insertProjectConfig(t, db, "accounts", `{"agentAccounts":{"codex":"project-P"},"worker":{"agent":"codex","agentAccountId":"role-R"},"agentRules":"keep"}`, false)
	insertProjectConfig(t, db, "empty-map", `{"agentAccounts":{}}`, false)
	insertProjectConfig(t, db, "archived", `{"agentAccounts":{"claude":"project-P"},"orchestrator":{"agentAccountId":"role-O"}}`, true)
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	config := func(id string) sql.NullString {
		t.Helper()
		var v sql.NullString
		if err := db.QueryRow(`SELECT config FROM projects WHERE id=?`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := config("null"); v.Valid {
		t.Fatalf("NULL config changed: %q", v.String)
	}
	if v := config("plain"); v.String != plain {
		t.Fatalf("config without agentAccounts changed: %q", v.String)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM projects WHERE json_valid(config) AND json_type(config,'$.agentAccounts') IS NOT NULL`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("agentAccounts left in %d configs (err %v)", remaining, err)
	}
	var role, rules, archivedRole string
	if err := db.QueryRow(`SELECT json_extract(config,'$.worker.agentAccountId'), json_extract(config,'$.agentRules') FROM projects WHERE id='accounts'`).Scan(&role, &rules); err != nil || role != "role-R" || rules != "keep" {
		t.Fatalf("other settings changed: role=%q rules=%q err=%v", role, rules, err)
	}
	if err := db.QueryRow(`SELECT json_extract(config,'$.orchestrator.agentAccountId') FROM projects WHERE id='archived'`).Scan(&archivedRole); err != nil || archivedRole != "role-O" {
		t.Fatalf("archived role changed: %q %v", archivedRole, err)
	}
	if v := config("empty-map"); v.String != "{}" {
		t.Fatalf("empty agentAccounts map not removed: %q", v.String)
	}
}

func TestRemoveProjectAgentAccountsStopsAndNamesAbnormalProjects(t *testing.T) {
	for _, tc := range []struct{ config, problem string }{
		{`{"agentAccounts":`, "config is not valid JSON"},
		{`["secret-canary"]`, "config is not a JSON object"},
		{`{"agentAccounts":"secret-canary"}`, "agentAccounts is not a JSON object"},
	} {
		t.Run(tc.problem, func(t *testing.T) {
			db := openMigratedDatabaseCopy(t, beforeRemoveProjectAgentAccounts)
			ordinary := `{"agentAccounts":{"codex":"project-P"}}`
			if tc.problem == "config is not valid JSON" {
				// The guardian insert trigger already refuses malformed config;
				// remove it from this copy to model a hand-edited database.
				if _, err := db.Exec(`DROP TRIGGER guardian_project_insert`); err != nil {
					t.Fatal(err)
				}
			}
			insertProjectConfig(t, db, "ordinary", ordinary, false)
			insertProjectConfig(t, db, "broken", tc.config, true)
			err := migrate(db)
			if err == nil {
				t.Fatal("startup continued past an abnormal project config")
			}
			if want := `project "broken": ` + tc.problem; !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q does not name %q", err, want)
			}
			if strings.Contains(err.Error(), "secret-canary") || strings.Contains(err.Error(), "ordinary") {
				t.Fatalf("error exposed config content or a healthy project: %v", err)
			}
			var applied int
			if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id=?`, removeProjectAgentAccountsVersion).Scan(&applied); err != nil || applied != 0 {
				t.Fatalf("cleanup migration recorded: %d %v", applied, err)
			}
			var got string
			if err := db.QueryRow(`SELECT config FROM projects WHERE id='ordinary'`).Scan(&got); err != nil || got != ordinary {
				t.Fatalf("stopped startup changed data: %q %v", got, err)
			}
		})
	}
}

func TestRemoveProjectAgentAccountsUpgradesSchemaWithoutProjectConfig(t *testing.T) {
	// 0008 adds projects.config; a database from before it must still upgrade.
	db := openMigratedDatabaseCopy(t, 7)
	if _, err := db.Exec(`INSERT INTO projects(id,path,registered_at) VALUES('old','/fake/old',CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("upgrade from v7: %v", err)
	}
	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM goose_db_version WHERE version_id=? AND is_applied=1`, removeProjectAgentAccountsVersion).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("cleanup migration not applied: %d %v", applied, err)
	}
}
