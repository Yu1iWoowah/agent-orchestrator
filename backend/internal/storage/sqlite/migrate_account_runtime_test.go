package sqlite

import (
	"testing"

	"github.com/pressly/goose/v3"
)

func TestAccountRuntimeMigrationPreservesUnknownLegacyAndBlocksUnsafeDowngrade(t *testing.T) {
	db := openMigratedDatabaseCopyNoForeignKeys(t, 20261029104125598)
	_, err := db.Exec(`INSERT INTO session_interface_transitions(id,session_id,source_mode,target_mode,policy,phase,source_account_id,target_account_id,created_at,updated_at) VALUES('legacy-runtime-gap','legacy-session','tui','tui','drain','target_starting','A','B',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatal(err)
	}
	upTo(t, db, 20261029163259629)
	var state string
	if err := db.QueryRow(`SELECT target_runtime_state FROM session_interface_transitions WHERE id='legacy-runtime-gap'`).Scan(&state); err != nil || state != "" {
		t.Fatalf("legacy uncertainty was invented as no-effect: %q %v", state, err)
	}
	gooseMu.Lock()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	err = goose.SetDialect("sqlite3")
	if err == nil {
		err = goose.DownTo(db, "migrations", 20261029104125598)
	}
	gooseMu.Unlock()
	if err == nil {
		t.Fatal("downgrade discarded active writer evidence")
	}
	if err := db.QueryRow(`SELECT target_runtime_state FROM session_interface_transitions WHERE id='legacy-runtime-gap'`).Scan(&state); err != nil {
		t.Fatal("failed downgrade modified journal", err)
	}
	if _, err := db.Exec(`UPDATE session_interface_transitions SET phase='completed' WHERE id='legacy-runtime-gap'`); err != nil {
		t.Fatal(err)
	}
	downTo(t, db, 20261029104125598)
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM session_interface_transitions WHERE id='legacy-runtime-gap'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("safe downgrade lost historical journal: count=%d %v", count, err)
	}
}
