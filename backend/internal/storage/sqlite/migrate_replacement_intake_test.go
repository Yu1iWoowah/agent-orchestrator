package sqlite

import "testing"

func TestReplacementMigrationBackfillsIntakeSequenceWithoutDroppingLegacyTurns(t *testing.T) {
	const version int64 = 20261028170455579
	db := openMigratedDatabaseCopyNoForeignKeys(t, version-1)
	mustExec(t, db, `
INSERT INTO projects (id, path, display_name, registered_at) VALUES ('p', '/tmp/p', 'P', '2026-10-28T00:00:00Z');
INSERT INTO sessions (id, project_id, num, harness, session_mode, activity_last_at, created_at, updated_at)
VALUES ('s', 'p', 1, 'claude-code', 'chat', '2026-10-28T00:00:00Z', '2026-10-28T00:00:00Z', '2026-10-28T00:00:00Z');
INSERT INTO conversations (id, scope, project_id, session_id, current_session_id, active_branch_id, latest_sequence, created_at, updated_at)
VALUES ('c', 'session', 'p', 's', 's', 'c:root', 9, '2026-10-28T00:00:00Z', '2026-10-28T00:00:00Z');
INSERT INTO conversation_branches (id, conversation_id, session_id, created_at)
VALUES ('c:root', 'c', 's', '2026-10-28T00:00:00Z');
INSERT INTO conversation_turns (id, conversation_id, handled_by_session_id, state, requested_at, branch_id)
VALUES ('queued', 'c', 's', 'queued', '2026-10-28T00:00:00Z', 'c:root'),
('imported', 'c', 's', 'recovered', '2026-10-28T00:00:00Z', 'c:root'),
('orphan', 'c', 's', 'failed', '2026-10-28T00:00:00Z', 'c:root');
INSERT INTO conversation_messages (id, conversation_id, turn_id, sequence, role, origin, text, created_at, updated_at, branch_id)
VALUES ('m', 'c', 'queued', 4, 'user', 'human', 'preserved queued text', '2026-10-28T00:00:00Z', '2026-10-28T00:00:00Z', 'c:root');
INSERT INTO conversation_activities (id, conversation_id, turn_id, sequence, kind, status, summary, created_at, updated_at, branch_id)
VALUES ('a1', 'c', 'queued', 8, 'command', 'completed', 'later', '2026-10-28T00:00:00Z', '2026-10-28T00:00:00Z', 'c:root'),
('a2', 'c', 'imported', 9, 'command', 'recovered', 'imported outcome', '2026-10-28T00:00:00Z', '2026-10-28T00:00:00Z', 'c:root');`)
	upTo(t, db, version)
	for id, want := range map[string]int64{"queued": 4, "imported": 9, "orphan": 0} {
		var got int64
		if err := db.QueryRow(`SELECT intake_sequence FROM conversation_turns WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s intake sequence = %d, want %d", id, got, want)
		}
	}
	var text string
	if err := db.QueryRow(`SELECT text FROM conversation_messages WHERE id = 'm'`).Scan(&text); err != nil || text != "preserved queued text" {
		t.Fatal(text, err)
	}
	downTo(t, db, version-1)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversation_turns`).Scan(&count); err != nil || count != 3 {
		t.Fatal(count, err)
	}
	upTo(t, db, version)
}
