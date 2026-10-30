package sqlite

import "testing"

func TestAccountRecoveryEvidenceMigrationPreservesReceiptsOnRoundTrip(t *testing.T) {
	db := openMigratedDatabaseCopyNoForeignKeys(t, 20261029165951329)
	if _, err := db.Exec(`
INSERT INTO projects(id,path,registered_at) VALUES('proof','/fake/project',CURRENT_TIMESTAMP);
INSERT INTO sessions(id,project_id,num,activity_last_at,created_at,updated_at)
 VALUES('proof-1','proof',1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
INSERT INTO session_interface_transitions(id,session_id,source_mode,target_mode,policy,phase,source_account_id,target_account_id,created_at,updated_at)
 VALUES('legacy','proof-1','tui','tui','drain','recovery_required','A','B',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		upTo(t, db, 20261029232658295)
		var retire int
		if err := db.QueryRow(`SELECT retire_without_stop FROM account_transition_recovery_evidence WHERE id='legacy'`).Scan(&retire); err != nil || retire != 1 {
			t.Fatalf("legacy confirmed-stop proof missing: %d %v", retire, err)
		}
		downTo(t, db, 20261029165951329)
		var state string
		if err := db.QueryRow(`SELECT target_runtime_state FROM session_interface_transitions WHERE id='legacy'`).Scan(&state); err != nil || state != "" {
			t.Fatalf("view migration rewrote historical evidence: %q %v", state, err)
		}
	}
}
