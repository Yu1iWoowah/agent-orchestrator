package sqlite

import (
	"reflect"
	"testing"
)

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

func TestMigrateAgentSettingsLeavesUnconvertedSessionsUnchanged(t *testing.T) {
	db := openMigratedDatabaseCopy(t, 20261028114415395)
	if _, err := db.Exec("INSERT INTO projects(id,path,registered_at) VALUES('settings','/fake',CURRENT_TIMESTAMP)"); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ id, mode, launch string }{
		{"empty-object", "tui", `{}`},
		{"false-booleans", "tui", `{"fastMode":false,"fastModeOff":false}`},
		{"other-choices", "tui", `{"agentMode":"plan"}`},
		{"empty-launch", "tui", ""},
		{"chat", "chat", `{"fastMode":true}`},
		{"ambiguous", "tui", `{"fastMode":true,"fastModeOff":true}`},
		{"malformed", "tui", "not-json"},
		{"numeric", "tui", `{"fastMode":1}`},
		{"numeric-other", "tui", `{"fastMode":true,"fastModeOff":0}`},
		{"null-other", "tui", `{"fastMode":true,"fastModeOff":null}`},
	}
	snapshot := func(id string) map[string]any {
		t.Helper()
		rows, err := db.Query("SELECT * FROM sessions WHERE id=?", id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if !rows.Next() {
			t.Fatalf("missing session %s: %v", id, rows.Err())
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		result := make(map[string]any, len(columns))
		for i, column := range columns {
			result[column] = values[i]
		}
		return result
	}
	before := make(map[string]map[string]any, len(cases))
	for i, tc := range cases {
		if _, err := db.Exec(`INSERT INTO sessions(id,project_id,num,harness,activity_last_at,created_at,updated_at,session_mode,model,effort,launch_options) VALUES(?,'settings',?,'codex',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,?,'saved-model','high',?)`, tc.id, i+1, tc.mode, tc.launch); err != nil {
			t.Fatal(err)
		}
		before[tc.id] = snapshot(tc.id)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			after := snapshot(tc.id)
			for column, value := range before[tc.id] {
				if got, exists := after[column]; !exists || !reflect.DeepEqual(value, got) {
					t.Errorf("unconverted %s changed: before=%#v after=%#v", column, value, after[column])
				}
			}
			if after["agent_fast_mode"] != "default" || after["agent_settings_revision"] != int64(0) {
				t.Errorf("invented preference: fast=%v revision=%v", after["agent_fast_mode"], after["agent_settings_revision"])
			}
		})
	}
}
