package sqlite

import "testing"

func TestMigrateAgentSettingsPreservesChoicesWithoutInventingChatFast(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261028114415395)
	if _, err := db.Exec("INSERT INTO projects(id,path,registered_at) VALUES('settings','/fake',CURRENT_TIMESTAMP)"); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct{ id, mode, launch, want string }{
		{"on", "tui", `{"fastMode":true}`, "on"},
		{"off", "tui", `{"fastModeOff":true}`, "off"},
		{"default", "tui", "", "default"},
		{"chat", "chat", `{"fastMode":true}`, "default"},
		{"ambiguous", "tui", `{"fastMode":true,"fastModeOff":true}`, "default"},
		{"malformed", "tui", "not-json", "default"},
		{"numeric", "tui", `{"fastMode":1}`, "default"},
		{"numeric-other", "tui", `{"fastMode":true,"fastModeOff":0}`, "default"},
		{"explicit-false", "tui", `{"fastMode":false,"fastModeOff":true}`, "off"},
	} {
		if _, err := db.Exec(`INSERT INTO sessions(id,project_id,num,harness,activity_last_at,created_at,updated_at,session_mode,model,effort,launch_options) VALUES(?,'settings',?,'codex',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,?,'saved-model','high',?)`, tc.id, i+1, tc.mode, tc.launch); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, want string }{{"on", "on"}, {"off", "off"}, {"default", "default"}, {"chat", "default"}, {"ambiguous", "default"}, {"malformed", "default"}, {"numeric", "default"}, {"numeric-other", "default"}, {"explicit-false", "off"}} {
		var mode, model, effort string
		var revision int64
		if err := db.QueryRow("SELECT agent_fast_mode,model,effort,agent_settings_revision FROM sessions WHERE id=?", tc.id).Scan(&mode, &model, &effort, &revision); err != nil {
			t.Fatal(err)
		}
		if mode != tc.want || model != "saved-model" || effort != "high" || revision != 0 {
			t.Fatalf("%s: %s/%s/%s/%d", tc.id, mode, model, effort, revision)
		}
	}
}
